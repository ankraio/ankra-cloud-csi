package cloud

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/ankraio/ankra-cloud-csi/internal/ankraapi"
)

// The operations the API is gaining alongside this driver: snapshots and a server's storage capacity. Each call
// names its operationId. Once `make sync-client` has regenerated the client from a specification that has the
// operation, the call goes through ankraapi.Client.Call with the specification's method and path; until then it is a
// plain HTTP request with the method and path below. Swapping to the typed methods later only touches this file.

type pendingRoute struct {
	operationID    string
	method         string
	path           string
	pathParameters []string
}

var (
	routeCreateSnapshot       = pendingRoute{"create_snapshot", http.MethodPost, "/v1/storages/{id}/snapshots", []string{"id"}}
	routeListStorageSnapshots = pendingRoute{"list_storage_snapshots", http.MethodGet, "/v1/storages/{id}/snapshots", []string{"id"}}
	routeGetSnapshot          = pendingRoute{"get_snapshot", http.MethodGet, "/v1/snapshots/{id}", []string{"id"}}
	routeDeleteSnapshot       = pendingRoute{"delete_snapshot", http.MethodDelete, "/v1/snapshots/{id}", []string{"id"}}
	routeGetStorageCapacity   = pendingRoute{"get_server_storage_capacity", http.MethodGet, "/v1/servers/{id}/storage-capacity", []string{"id"}}
)

type pendingTransport struct {
	endpoint   *url.URL
	token      string
	userAgent  string
	httpClient *http.Client
	generated  *ankraapi.Client
}

func newPendingTransport(endpoint string, token string, userAgent string, httpClient *http.Client, generated *ankraapi.Client) (*pendingTransport, error) {
	parsed, parseError := url.Parse(strings.TrimRight(endpoint, "/"))
	if parseError != nil {
		return nil, fmt.Errorf("parse endpoint: %w", parseError)
	}
	return &pendingTransport{endpoint: parsed, token: token, userAgent: userAgent, httpClient: httpClient, generated: generated}, nil
}

type snapshotDocument struct {
	ID            string    `json:"id"`
	StorageID     string    `json:"storage_id"`
	Zone          string    `json:"zone"`
	Title         string    `json:"title"`
	State         string    `json:"state"`
	SizeGibibytes int64     `json:"size_gibibytes"`
	CreatedAt     time.Time `json:"created_at"`
}

func (document snapshotDocument) snapshot() Snapshot {
	return Snapshot(document)
}

type operationEnvelope struct {
	Operation ankraapi.Operation `json:"operation"`
}

func (transport *pendingTransport) createSnapshot(ctx context.Context, volumeID string, title string) (Snapshot, Operation, error) {
	var answer struct {
		Snapshot  snapshotDocument   `json:"snapshot"`
		Operation ankraapi.Operation `json:"operation"`
	}
	body := map[string]any{"title": title}
	if callError := transport.call(ctx, routeCreateSnapshot, map[string]string{"id": volumeID}, nil, body, &answer); callError != nil {
		return Snapshot{}, Operation{}, callError
	}
	return answer.Snapshot.snapshot(), operationFromAPI(answer.Operation), nil
}

func (transport *pendingTransport) listVolumeSnapshots(ctx context.Context, volumeID string) ([]Snapshot, error) {
	var snapshots []Snapshot
	query := url.Values{"limit": {"100"}}
	for {
		var page struct {
			Items      []snapshotDocument `json:"items"`
			NextCursor *string            `json:"next_cursor"`
		}
		if callError := transport.call(ctx, routeListStorageSnapshots, map[string]string{"id": volumeID}, query, nil, &page); callError != nil {
			return nil, callError
		}
		for _, document := range page.Items {
			snapshots = append(snapshots, document.snapshot())
		}
		if page.NextCursor == nil || *page.NextCursor == "" {
			return snapshots, nil
		}
		query = url.Values{"limit": {"100"}, "cursor": {*page.NextCursor}}
	}
}

func (transport *pendingTransport) getSnapshot(ctx context.Context, snapshotID string) (Snapshot, error) {
	var answer struct {
		Snapshot snapshotDocument `json:"snapshot"`
	}
	if callError := transport.call(ctx, routeGetSnapshot, map[string]string{"id": snapshotID}, nil, nil, &answer); callError != nil {
		return Snapshot{}, callError
	}
	return answer.Snapshot.snapshot(), nil
}

func (transport *pendingTransport) deleteSnapshot(ctx context.Context, snapshotID string) (Operation, error) {
	var answer operationEnvelope
	if callError := transport.call(ctx, routeDeleteSnapshot, map[string]string{"id": snapshotID}, nil, nil, &answer); callError != nil {
		return Operation{}, callError
	}
	return operationFromAPI(answer.Operation), nil
}

type tierCapacityDocument struct {
	Tier                 string  `json:"tier"`
	MaximumSizeGibibytes int64   `json:"maximum_size_gibibytes"`
	AvailableGibibytes   *int64  `json:"available_gibibytes"`
	Refusal              *string `json:"refusal"`
}

func (transport *pendingTransport) getStorageCapacity(ctx context.Context, serverID string) (StorageCapacity, error) {
	var answer struct {
		StorageCapacity struct {
			ServerID    string                 `json:"server_id"`
			Zone        string                 `json:"zone"`
			DefaultTier string                 `json:"default_tier"`
			Tiers       []tierCapacityDocument `json:"tiers"`
		} `json:"storage_capacity"`
	}
	if callError := transport.call(ctx, routeGetStorageCapacity, map[string]string{"id": serverID}, nil, nil, &answer); callError != nil {
		return StorageCapacity{}, callError
	}
	document := answer.StorageCapacity
	capacity := StorageCapacity{ServerID: document.ServerID, Zone: document.Zone, DefaultTier: document.DefaultTier,
		Tiers: make([]TierCapacity, 0, len(document.Tiers))}
	for _, tier := range document.Tiers {
		converted := TierCapacity{Tier: tier.Tier, MaximumGibibytes: tier.MaximumSizeGibibytes, AvailableGibibytes: tier.AvailableGibibytes}
		if tier.Refusal != nil {
			converted.Refusal = *tier.Refusal
		}
		capacity.Tiers = append(capacity.Tiers, converted)
	}
	return capacity, nil
}

// call runs one pending operation and decodes its JSON answer into result.
func (transport *pendingTransport) call(ctx context.Context, route pendingRoute, pathParameters map[string]string, query url.Values, body any, result any) error {
	var encoded []byte
	if body != nil {
		marshalled, marshalError := json.Marshal(body)
		if marshalError != nil {
			return fmt.Errorf("%s: encode the body: %w", route.operationID, marshalError)
		}
		encoded = marshalled
	}
	var answer []byte
	if _, isGenerated := ankraapi.Operations[route.operationID]; isGenerated {
		response, callError := transport.generated.Call(ctx, route.operationID, pathParameters, query, encoded)
		if callError != nil {
			return callError
		}
		answer = response.Body
	} else {
		response, callError := transport.send(ctx, route, pathParameters, query, encoded)
		if callError != nil {
			return callError
		}
		answer = response
	}
	if len(bytes.TrimSpace(answer)) == 0 {
		return nil
	}
	if decodeError := json.Unmarshal(answer, result); decodeError != nil {
		return fmt.Errorf("%s: decode the answer: %w", route.operationID, decodeError)
	}
	return nil
}

func (transport *pendingTransport) send(ctx context.Context, route pendingRoute, pathParameters map[string]string, query url.Values, body []byte) ([]byte, error) {
	path := route.path
	for _, name := range route.pathParameters {
		value := pathParameters[name]
		if value == "" {
			return nil, fmt.Errorf("%s needs the path parameter %s", route.operationID, name)
		}
		path = strings.ReplaceAll(path, "{"+name+"}", url.PathEscape(value))
	}
	target := transport.endpoint.JoinPath(path)
	if len(query) > 0 {
		target.RawQuery = query.Encode()
	}
	var payload io.Reader
	if body != nil {
		payload = bytes.NewReader(body)
	}
	request, requestError := http.NewRequestWithContext(ctx, route.method, target.String(), payload)
	if requestError != nil {
		return nil, fmt.Errorf("%s: build the request: %w", route.operationID, requestError)
	}
	if body != nil {
		request.Header.Set("Content-Type", "application/json")
	}
	request.Header.Set("Accept", "application/json")
	request.Header.Set("User-Agent", transport.userAgent)
	request.Header.Set("Authorization", "Bearer "+transport.token)
	response, sendError := transport.httpClient.Do(request)
	if sendError != nil {
		return nil, fmt.Errorf("%s: %w", route.operationID, sendError)
	}
	defer func() { _ = response.Body.Close() }()
	content, readError := io.ReadAll(io.LimitReader(response.Body, 16<<20))
	if readError != nil {
		return nil, fmt.Errorf("%s: read the answer: %w", route.operationID, readError)
	}
	if response.StatusCode >= 400 {
		apiError := &ankraapi.Error{StatusCode: response.StatusCode, Title: http.StatusText(response.StatusCode), Body: content}
		var problem struct {
			Title  string `json:"title"`
			Detail string `json:"detail"`
		}
		if json.Unmarshal(content, &problem) == nil {
			if problem.Title != "" {
				apiError.Title = problem.Title
			}
			apiError.Detail = problem.Detail
		}
		return nil, apiError
	}
	return content, nil
}

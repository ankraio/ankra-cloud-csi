package cloud

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"errors"
	"fmt"
	"net/http"
	"os"
	"time"

	"github.com/ankraio/ankra-cloud-csi/internal/ankraapi"
)

// Configuration is how the driver reaches the API.
type Configuration struct {
	// Endpoint is the API's base URL, for example https://api.ankra.cloud (ANKRA_CLOUD_API_URL).
	Endpoint string
	// Token is a customer API token (ANKRA_CLOUD_TOKEN).
	Token string
	// CABundlePath optionally names a PEM file of extra roots to trust (ANKRA_CLOUD_CA_BUNDLE).
	CABundlePath string
	// UserAgent identifies the driver in the API's logs.
	UserAgent string
	Timeout   time.Duration
}

// Client implements API against the Ankra Cloud public API.
type Client struct {
	generated *ankraapi.Client
	pending   *pendingTransport
}

var _ API = (*Client)(nil)

// NewClient builds a Client from the configuration.
func NewClient(configuration Configuration) (*Client, error) {
	if configuration.Endpoint == "" {
		return nil, errors.New("the API URL is required (ANKRA_CLOUD_API_URL)")
	}
	if configuration.Token == "" {
		return nil, errors.New("an API token is required (ANKRA_CLOUD_TOKEN)")
	}
	timeout := configuration.Timeout
	if timeout <= 0 {
		timeout = 60 * time.Second
	}
	transport := http.DefaultTransport.(*http.Transport).Clone()
	if configuration.CABundlePath != "" {
		bundle, readError := os.ReadFile(configuration.CABundlePath)
		if readError != nil {
			return nil, fmt.Errorf("read the CA bundle: %w", readError)
		}
		roots, rootsError := x509.SystemCertPool()
		if rootsError != nil || roots == nil {
			roots = x509.NewCertPool()
		}
		if !roots.AppendCertsFromPEM(bundle) {
			return nil, fmt.Errorf("the CA bundle %s holds no PEM certificate", configuration.CABundlePath)
		}
		transport.TLSClientConfig = &tls.Config{RootCAs: roots, MinVersion: tls.VersionTLS12}
	}
	httpClient := &http.Client{Timeout: timeout, Transport: transport,
		CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
	userAgent := configuration.UserAgent
	if userAgent == "" {
		userAgent = "ankra-cloud-csi"
	}
	generated, clientError := ankraapi.New(configuration.Endpoint, ankraapi.APITokenCredential(configuration.Token),
		ankraapi.WithHTTPClient(httpClient), ankraapi.WithUserAgent(userAgent))
	if clientError != nil {
		return nil, clientError
	}
	pending, pendingError := newPendingTransport(configuration.Endpoint, configuration.Token, userAgent, httpClient, generated)
	if pendingError != nil {
		return nil, pendingError
	}
	return &Client{generated: generated, pending: pending}, nil
}

// ListVolumes returns every storage of the account, following the cursor to the last page.
func (client *Client) ListVolumes(ctx context.Context) ([]Volume, error) {
	var volumes []Volume
	var cursor *string
	limit := int64(100)
	for {
		page, listError := client.generated.ListStorages(ctx, ankraapi.ListStoragesParameters{Cursor: cursor, Limit: &limit})
		if listError != nil {
			return nil, listError
		}
		for _, storage := range page.Items {
			volumes = append(volumes, volumeFromStorage(storage))
		}
		if page.NextCursor == nil || *page.NextCursor == "" {
			return volumes, nil
		}
		next := *page.NextCursor
		cursor = &next
	}
}

// GetVolume reads one storage.
func (client *Client) GetVolume(ctx context.Context, volumeID string) (Volume, error) {
	envelope, getError := client.generated.GetStorage(ctx, ankraapi.GetStorageParameters{ID: volumeID})
	if getError != nil {
		return Volume{}, getError
	}
	return volumeFromStorage(envelope.Storage), nil
}

// CreateVolume creates a storage. An empty tier lets the API use the zone's default storage tier.
func (client *Client) CreateVolume(ctx context.Context, request CreateVolumeRequest) (Volume, Operation, error) {
	body := ankraapi.CreateStorageRequest{Zone: request.Zone, Title: request.Title}
	if request.Tier != "" {
		tier := request.Tier
		body.Tier = &tier
	}
	if request.SizeGibibytes > 0 {
		size := request.SizeGibibytes
		body.SizeGibibytes = &size
	}
	if request.SourceStorageID != "" {
		source := request.SourceStorageID
		body.SourceStorageID = &source
	}
	if request.SourceSnapshotID != "" {
		source := request.SourceSnapshotID
		body.SourceSnapshotID = &source
	}
	if request.PlacementServerID != "" {
		server := request.PlacementServerID
		body.Placement = &ankraapi.StoragePlacement{ServerID: &server}
	}
	created, createError := client.generated.CreateStorage(ctx, body)
	if createError != nil {
		return Volume{}, Operation{}, createError
	}
	return volumeFromStorage(created.Storage), operationFromAPI(created.Operation), nil
}

// DeleteVolume deletes a storage.
func (client *Client) DeleteVolume(ctx context.Context, volumeID string) (Operation, error) {
	envelope, deleteError := client.generated.DeleteStorage(ctx, ankraapi.DeleteStorageParameters{ID: volumeID})
	if deleteError != nil {
		return Operation{}, deleteError
	}
	return operationFromAPI(envelope.Operation), nil
}

// AttachVolume attaches a storage to a server (hot-plug when the server runs).
func (client *Client) AttachVolume(ctx context.Context, volumeID string, serverID string) (Operation, error) {
	envelope, attachError := client.generated.AttachStorage(ctx, ankraapi.AttachStorageParameters{ID: volumeID},
		ankraapi.AttachStorageRequest{ServerID: serverID})
	if attachError != nil {
		return Operation{}, attachError
	}
	return operationFromAPI(envelope.Operation), nil
}

// DetachVolume detaches a storage from its server (hot-unplug when the server runs).
func (client *Client) DetachVolume(ctx context.Context, volumeID string) (Operation, error) {
	envelope, detachError := client.generated.DetachStorage(ctx, ankraapi.DetachStorageParameters{ID: volumeID})
	if detachError != nil {
		return Operation{}, detachError
	}
	return operationFromAPI(envelope.Operation), nil
}

// ResizeVolume grows a storage.
func (client *Client) ResizeVolume(ctx context.Context, volumeID string, sizeGibibytes int64) (Operation, error) {
	envelope, resizeError := client.generated.ResizeStorage(ctx, ankraapi.ResizeStorageParameters{ID: volumeID},
		ankraapi.ResizeStorageRequest{SizeGibibytes: sizeGibibytes})
	if resizeError != nil {
		return Operation{}, resizeError
	}
	return operationFromAPI(envelope.Operation), nil
}

// CreateSnapshot snapshots a storage (create_snapshot).
func (client *Client) CreateSnapshot(ctx context.Context, volumeID string, title string) (Snapshot, Operation, error) {
	return client.pending.createSnapshot(ctx, volumeID, title)
}

// ListVolumeSnapshots lists a storage's snapshots (list_storage_snapshots).
func (client *Client) ListVolumeSnapshots(ctx context.Context, volumeID string) ([]Snapshot, error) {
	return client.pending.listVolumeSnapshots(ctx, volumeID)
}

// GetSnapshot reads one snapshot (get_snapshot).
func (client *Client) GetSnapshot(ctx context.Context, snapshotID string) (Snapshot, error) {
	return client.pending.getSnapshot(ctx, snapshotID)
}

// DeleteSnapshot deletes a snapshot (delete_snapshot).
func (client *Client) DeleteSnapshot(ctx context.Context, snapshotID string) (Operation, error) {
	return client.pending.deleteSnapshot(ctx, snapshotID)
}

// GetServer reads a server.
func (client *Client) GetServer(ctx context.Context, serverID string) (Server, error) {
	envelope, getError := client.generated.GetServer(ctx, ankraapi.GetServerParameters{ID: serverID})
	if getError != nil {
		return Server{}, getError
	}
	return Server{ID: envelope.Server.ID, Zone: envelope.Server.Zone, State: envelope.Server.State}, nil
}

// GetOperation reads an operation.
func (client *Client) GetOperation(ctx context.Context, operationID string) (Operation, error) {
	envelope, getError := client.generated.GetOperation(ctx, ankraapi.GetOperationParameters{ID: operationID})
	if getError != nil {
		return Operation{}, getError
	}
	return operationFromAPI(envelope.Operation), nil
}

func volumeFromStorage(storage ankraapi.Storage) Volume {
	volume := Volume{ID: storage.ID, Zone: storage.Zone, Title: storage.Title, Tier: storage.Tier, State: storage.State,
		SizeGibibytes: storage.SizeGibibytes, DeviceSerial: DeviceSerialFor(storage.ID), CreatedAt: storage.CreatedAt}
	if storage.ServerID != nil {
		volume.ServerID = *storage.ServerID
	}
	if storage.ActiveOperation != nil {
		volume.ActiveOperationID = storage.ActiveOperation.ID
	}
	return volume
}

func operationFromAPI(operation ankraapi.Operation) Operation {
	return Operation{ID: operation.ID, Status: operation.Status, Step: operation.Step, Error: operation.Error}
}

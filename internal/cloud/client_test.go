package cloud

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"
	"time"
)

type recordedCall struct {
	method        string
	path          string
	authorization string
	body          map[string]any
}

// newAPIServer answers every route with the canned JSON for "METHOD path" and records each call.
func newAPIServer(t *testing.T, answers map[string]string) (*Client, *[]recordedCall) {
	t.Helper()
	var mutex sync.Mutex
	var calls []recordedCall
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		content, _ := io.ReadAll(request.Body)
		call := recordedCall{method: request.Method, path: request.URL.Path, authorization: request.Header.Get("Authorization")}
		if len(content) > 0 {
			_ = json.Unmarshal(content, &call.body)
		}
		mutex.Lock()
		calls = append(calls, call)
		mutex.Unlock()
		answer, isKnown := answers[request.Method+" "+request.URL.Path]
		if !isKnown {
			writer.Header().Set("Content-Type", "application/problem+json")
			writer.WriteHeader(http.StatusNotFound)
			_, _ = io.WriteString(writer, `{"title":"Not Found","detail":"no such thing"}`)
			return
		}
		writer.Header().Set("Content-Type", "application/json")
		if request.Method == http.MethodPost || request.Method == http.MethodDelete {
			writer.WriteHeader(http.StatusAccepted)
		}
		_, _ = io.WriteString(writer, answer)
	}))
	t.Cleanup(server.Close)
	client, clientError := NewClient(Configuration{Endpoint: server.URL, Token: "act_test", Timeout: 5 * time.Second})
	if clientError != nil {
		t.Fatalf("NewClient: %v", clientError)
	}
	return client, &calls
}

const storageJSON = `{"id":"01a0d058-e588-7fff-8000-000000000555","zone":"fsn1","title":"pvc-1","tier":"standard","state":"online",
"size_gibibytes":10,"server_id":"srv-1","device_index":1,"active_operation":null,"backup_rule":null,"next_backup_at":null,
"source_template":null,"created_at":"2026-09-28T12:00:00Z","updated_at":"2026-09-28T12:00:00Z"}`

const operationJSON = `{"id":"op-1","kind":"storage.create","status":"running","step":"create_disk","step_count":2,"step_index":0,
"error":"","created_at":"2026-09-28T12:00:00Z","deadline_at":"2026-09-28T13:00:00Z","backup_id":null,"finished_at":null,
"floating_ip_id":null,"router_id":null,"server_id":null,"started_at":null,"storage_id":null,"template_id":null}`

const snapshotJSON = `{"id":"snap-1","storage_id":"01a0d058-e588-7fff-8000-000000000555","zone":"fsn1","title":"snap-a","state":"online",
"size_gibibytes":10,"created_at":"2026-09-28T12:00:00Z"}`

func TestGeneratedOperations(t *testing.T) {
	client, calls := newAPIServer(t, map[string]string{
		"GET /v1/storages":                  `{"items":[` + storageJSON + `],"next_cursor":null}`,
		"GET /v1/storages/vol-1":            `{"storage":` + storageJSON + `}`,
		"POST /v1/storages":                 `{"storage":` + storageJSON + `,"operation":` + operationJSON + `}`,
		"POST /v1/storages/vol-1/attach":    `{"operation":` + operationJSON + `}`,
		"POST /v1/storages/vol-1/detach":    `{"operation":` + operationJSON + `}`,
		"POST /v1/storages/vol-1/resize":    `{"operation":` + operationJSON + `}`,
		"DELETE /v1/storages/vol-1":         `{"operation":` + operationJSON + `}`,
		"GET /v1/operations/op-1":           `{"operation":` + operationJSON + `}`,
		"GET /v1/servers/01a0d058-e588-srv": `{"server":{"id":"01a0d058-e588-srv","zone":"fsn1","state":"running","labels":{}}}`,
	})
	ctx := context.Background()
	volumes, listError := client.ListVolumes(ctx)
	if listError != nil || len(volumes) != 1 {
		t.Fatalf("ListVolumes: %v %v", volumes, listError)
	}
	volume := volumes[0]
	if volume.DeviceSerial != "01a0d058-e588-7fff-8" || volume.ServerID != "srv-1" || volume.SizeGibibytes != 10 {
		t.Fatalf("volume %+v", volume)
	}
	if _, getError := client.GetVolume(ctx, "vol-1"); getError != nil {
		t.Fatalf("GetVolume: %v", getError)
	}
	_, operation, createError := client.CreateVolume(ctx, CreateVolumeRequest{Zone: "fsn1", Title: "pvc-1", Tier: "standard", SizeGibibytes: 10})
	if createError != nil || operation.ID != "op-1" || operation.Status != OperationStatusRunning {
		t.Fatalf("CreateVolume: %+v %v", operation, createError)
	}
	for _, call := range []func() (Operation, error){
		func() (Operation, error) { return client.AttachVolume(ctx, "vol-1", "srv-1") },
		func() (Operation, error) { return client.DetachVolume(ctx, "vol-1") },
		func() (Operation, error) { return client.ResizeVolume(ctx, "vol-1", 20) },
		func() (Operation, error) { return client.DeleteVolume(ctx, "vol-1") },
		func() (Operation, error) { return client.GetOperation(ctx, "op-1") },
	} {
		if _, callError := call(); callError != nil {
			t.Fatalf("call: %v", callError)
		}
	}
	server, serverError := client.GetServer(ctx, "01a0d058-e588-srv")
	if serverError != nil || server.Zone != "fsn1" {
		t.Fatalf("GetServer: %+v %v", server, serverError)
	}
	for _, call := range *calls {
		if call.authorization != "Bearer act_test" {
			t.Fatalf("%s %s sent %q", call.method, call.path, call.authorization)
		}
	}
	create := (*calls)[2]
	if create.body["title"] != "pvc-1" || create.body["tier"] != "standard" || create.body["size_gibibytes"] != float64(10) {
		t.Fatalf("create body %v", create.body)
	}
	if _, hasLabels := create.body["labels"]; hasLabels {
		t.Fatal("create_storage does not accept labels yet")
	}
	attach := (*calls)[3]
	if attach.body["server_id"] != "srv-1" {
		t.Fatalf("attach body %v", attach.body)
	}
	resize := (*calls)[5]
	if resize.body["size_gibibytes"] != float64(20) {
		t.Fatalf("resize body %v", resize.body)
	}
}

func TestPendingOperationsUseTheirRoutes(t *testing.T) {
	client, calls := newAPIServer(t, map[string]string{
		"POST /v1/storages/vol-1/snapshots": `{"snapshot":` + snapshotJSON + `,"operation":` + operationJSON + `}`,
		"GET /v1/storages/vol-1/snapshots":  `{"items":[` + snapshotJSON + `],"next_cursor":null}`,
		"GET /v1/snapshots/snap-1":          `{"snapshot":` + snapshotJSON + `}`,
		"DELETE /v1/snapshots/snap-1":       `{"operation":` + operationJSON + `}`,
		"POST /v1/storages":                 `{"storage":` + storageJSON + `,"operation":` + operationJSON + `}`,
	})
	ctx := context.Background()
	snapshot, _, createError := client.CreateSnapshot(ctx, "vol-1", "snap-a")
	if createError != nil || snapshot.ID != "snap-1" || !snapshot.IsReady() || snapshot.SizeGibibytes != 10 {
		t.Fatalf("CreateSnapshot: %+v %v", snapshot, createError)
	}
	listed, listError := client.ListVolumeSnapshots(ctx, "vol-1")
	if listError != nil || len(listed) != 1 {
		t.Fatalf("ListVolumeSnapshots: %v %v", listed, listError)
	}
	if _, getError := client.GetSnapshot(ctx, "snap-1"); getError != nil {
		t.Fatalf("GetSnapshot: %v", getError)
	}
	if _, deleteError := client.DeleteSnapshot(ctx, "snap-1"); deleteError != nil {
		t.Fatalf("DeleteSnapshot: %v", deleteError)
	}
	if _, _, restoreError := client.CreateVolume(ctx, CreateVolumeRequest{Zone: "fsn1", Title: "pvc-2", Tier: "standard",
		SourceSnapshotID: "snap-1"}); restoreError != nil {
		t.Fatalf("CreateVolume from a snapshot: %v", restoreError)
	}
	if (*calls)[0].body["title"] != "snap-a" {
		t.Fatalf("snapshot body %v", (*calls)[0].body)
	}
	restore := (*calls)[4]
	if restore.path != "/v1/storages" || restore.body["source_snapshot_id"] != "snap-1" || restore.authorization != "Bearer act_test" {
		t.Fatalf("restore call %+v", restore)
	}
	if _, hasSize := restore.body["size_gibibytes"]; hasSize {
		t.Fatal("a restore without a size must let the API use the snapshot's")
	}

	_, missingError := client.GetSnapshot(ctx, "absent")
	if !IsNotFound(missingError) {
		t.Fatalf("missing snapshot error %v", missingError)
	}
}

func TestWaitForOperation(t *testing.T) {
	client, _ := newAPIServer(t, map[string]string{
		"GET /v1/operations/op-1": `{"operation":` + operationJSON + `}`,
	})
	ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel()
	_, waitError := WaitForOperation(ctx, client, Operation{ID: "op-1", Status: OperationStatusRunning}, 5*time.Millisecond)
	if waitError == nil {
		t.Fatal("a running operation must time out")
	}
	failed, failedError := WaitForOperation(context.Background(), client, Operation{ID: "op-2", Status: OperationStatusFailed, Error: "boom"}, time.Millisecond)
	if failedError == nil || failed.Error != "boom" {
		t.Fatalf("failed operation: %v", failedError)
	}
	if _, noneError := WaitForOperation(context.Background(), client, Operation{}, time.Millisecond); noneError != nil {
		t.Fatalf("no operation: %v", noneError)
	}
}

func TestNewClientNeedsAnEndpointAndAToken(t *testing.T) {
	if _, clientError := NewClient(Configuration{Token: "act_x"}); clientError == nil {
		t.Fatal("no endpoint accepted")
	}
	if _, clientError := NewClient(Configuration{Endpoint: "https://api.ankra.cloud"}); clientError == nil {
		t.Fatal("no token accepted")
	}
	if _, clientError := NewClient(Configuration{Endpoint: "https://api.ankra.cloud", Token: "act_x", CABundlePath: "/no/such/bundle.pem"}); clientError == nil {
		t.Fatal("a missing CA bundle accepted")
	}
}

func TestDeviceSerialFor(t *testing.T) {
	if serial := DeviceSerialFor("01a0d058-e588-7fff-8000-000000000555"); serial != "01a0d058-e588-7fff-8" {
		t.Fatalf("serial %q", serial)
	}
	if serial := DeviceSerialFor("short"); serial != "short" {
		t.Fatalf("serial %q", serial)
	}
}

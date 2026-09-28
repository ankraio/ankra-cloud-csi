package driver

import (
	"context"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/kubernetes-csi/csi-test/v5/pkg/sanity"

	"github.com/ankraio/ankra-cloud-csi/internal/cloud"
	"github.com/ankraio/ankra-cloud-csi/internal/cloud/cloudfake"
)

const (
	testZone     = "fsn1"
	testServerID = "01a0d058-e588-7fff-8000-000000000101"
)

func newTestDriver(t *testing.T, api *cloudfake.API, host *fakeHost) *Driver {
	t.Helper()
	api.AddServer(cloud.Server{ID: testServerID, Zone: testZone, State: "running"})
	csiDriver, driverError := New(Options{
		Mode: ModeAll, Version: "test", API: api, DefaultZone: testZone, Host: host,
		ResolveNodeIdentity: func(context.Context) (NodeIdentity, error) {
			return NodeIdentity{ServerID: testServerID, Zone: testZone}, nil
		},
		OperationPollInterval: time.Millisecond, OperationTimeout: 5 * time.Second, DeviceWaitTimeout: time.Millisecond,
		Logger: slog.New(slog.NewTextHandler(io.Discard, nil)),
	})
	if driverError != nil {
		t.Fatalf("New: %v", driverError)
	}
	return csiDriver
}

// TestSanity runs the csi-test sanity suite in-process against the driver, the fake API and the fake host.
func TestSanity(t *testing.T) {
	// Unix socket paths are short on macOS, so the socket lives under /tmp rather than t.TempDir().
	directory, mkdirError := os.MkdirTemp("/tmp", "ankra-csi-")
	if mkdirError != nil {
		t.Fatalf("temp dir: %v", mkdirError)
	}
	t.Cleanup(func() { _ = os.RemoveAll(directory) })
	socket := filepath.Join(directory, "csi.sock")
	csiDriver := newTestDriver(t, cloudfake.New(), newFakeHost())
	listener, listenError := Listen("unix://" + socket)
	if listenError != nil {
		t.Fatalf("listen: %v", listenError)
	}
	server := csiDriver.NewServer()
	go func() { _ = server.Serve(listener) }()
	t.Cleanup(server.Stop)

	configuration := sanity.NewTestConfig()
	configuration.Address = "unix:" + socket
	configuration.TargetPath = filepath.Join(directory, "target")
	configuration.StagingPath = filepath.Join(directory, "staging")
	configuration.TestVolumeSize = 10 * gibibyte
	configuration.TestVolumeExpandSize = 11 * gibibyte
	configuration.IdempotentCount = 3
	sanity.Test(t, configuration)
}

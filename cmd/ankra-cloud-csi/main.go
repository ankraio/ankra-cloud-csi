// Command ankra-cloud-csi is the CSI driver csi.ankra.cloud: the controller plugin (-mode controller, one replica
// next to the external-provisioner, -attacher, -resizer and -snapshotter sidecars) and the node plugin (-mode node, a
// DaemonSet next to node-driver-registrar).
package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"log/slog"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/ankraio/ankra-cloud-csi/internal/cloud"
	"github.com/ankraio/ankra-cloud-csi/internal/driver"
	"github.com/ankraio/ankra-cloud-csi/internal/metadata"
)

// version is set at build time with -ldflags "-X main.version=…".
var version = "dev"

func main() {
	if runError := run(); runError != nil {
		fmt.Fprintln(os.Stderr, "ankra-cloud-csi:", runError)
		os.Exit(1)
	}
}

func run() error {
	endpoint := flag.String("endpoint", "unix:///csi/csi.sock", "CSI endpoint (unix:// or tcp://)")
	mode := flag.String("mode", string(driver.ModeAll), "controller, node or all")
	defaultZone := flag.String("default-zone", os.Getenv("ANKRA_CLOUD_ZONE"),
		"zone of volumes whose request has no topology; the controller's own zone when empty")
	serverID := flag.String("server-id", os.Getenv("ANKRA_CLOUD_SERVER_ID"),
		"this node's server id; read from the metadata service when empty")
	maximumVolumes := flag.Int64("max-volumes-per-node", driver.DefaultMaximumVolumesPerNode, "volumes one node can attach")
	operationTimeout := flag.Duration("operation-timeout", 5*time.Minute, "how long one call waits for an API operation")
	pollInterval := flag.Duration("operation-poll-interval", 2*time.Second, "how often a running API operation is polled")
	showVersion := flag.Bool("version", false, "print the version and exit")
	flag.Parse()
	if *showVersion {
		fmt.Println(version)
		return nil
	}

	logger := slog.New(slog.NewJSONHandler(os.Stderr, nil)).With("driver", driver.Name, "mode", *mode)
	api, clientError := cloud.NewClient(cloud.Configuration{
		Endpoint:     os.Getenv("ANKRA_CLOUD_API_URL"),
		Token:        os.Getenv("ANKRA_CLOUD_TOKEN"),
		CABundlePath: os.Getenv("ANKRA_CLOUD_CA_BUNDLE"),
		UserAgent:    "ankra-cloud-csi/" + version,
	})
	if clientError != nil {
		return clientError
	}
	metadataClient := metadata.NewClient()
	resolveIdentity := func(ctx context.Context) (driver.NodeIdentity, error) {
		identifier := *serverID
		if identifier == "" {
			fromMetadata, metadataError := metadataClient.ServerID(ctx)
			if metadataError != nil {
				return driver.NodeIdentity{}, metadataError
			}
			identifier = fromMetadata
		}
		server, serverError := api.GetServer(ctx, identifier)
		if serverError != nil {
			return driver.NodeIdentity{}, fmt.Errorf("read server %s: %w", identifier, serverError)
		}
		return driver.NodeIdentity{ServerID: server.ID, Zone: server.Zone}, nil
	}
	zone := *defaultZone
	selectedMode := driver.Mode(*mode)
	if zone == "" && (selectedMode == driver.ModeController || selectedMode == driver.ModeAll) {
		ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		identity, identityError := resolveIdentity(ctx)
		cancel()
		if identityError != nil {
			logger.Warn("no default zone: volumes need a topology requirement", "error", identityError)
		} else {
			zone = identity.Zone
		}
	}
	options := driver.Options{
		Mode: selectedMode, Version: version, API: api, DefaultZone: zone, ResolveNodeIdentity: resolveIdentity,
		MaximumVolumesPerNode: *maximumVolumes, OperationTimeout: *operationTimeout, OperationPollInterval: *pollInterval,
		Logger: logger,
	}
	if selectedMode == driver.ModeNode || selectedMode == driver.ModeAll {
		options.Host = driver.NewHostOS()
	}
	csiDriver, driverError := driver.New(options)
	if driverError != nil {
		return driverError
	}
	listener, listenError := driver.Listen(*endpoint)
	if listenError != nil {
		return listenError
	}
	server := csiDriver.NewServer()
	signals := make(chan os.Signal, 1)
	signal.Notify(signals, syscall.SIGINT, syscall.SIGTERM)
	go func() {
		<-signals
		logger.Info("stopping")
		server.GracefulStop()
	}()
	logger.Info("serving", "endpoint", *endpoint, "version", version, "default_zone", zone)
	if serveError := server.Serve(listener); serveError != nil && !errors.Is(serveError, context.Canceled) {
		return serveError
	}
	return nil
}

// Package driver is the CSI driver csi.ankra.cloud: the identity, controller and node services over the Ankra Cloud
// API (cloud.API) and the node's mounts and block devices (Host).
package driver

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/container-storage-interface/spec/lib/go/csi"
	"google.golang.org/grpc"

	"github.com/ankraio/ankra-cloud-csi/internal/cloud"
)

const (
	// Name is the CSI driver name, the provisioner of the StorageClasses.
	Name = "csi.ankra.cloud"

	// TopologyZoneKey carries the zone of a volume and of a node.
	TopologyZoneKey = "topology.ankra.cloud/zone"
	// TopologyNodeKey carries the server id of a node; only local-nvme volumes are pinned to it.
	TopologyNodeKey = "topology.ankra.cloud/node"

	// VolumeNameLabel is the storage label that records the CSI volume name.
	VolumeNameLabel = "csi.ankra.cloud/volume-name"

	// ParameterTier is the StorageClass parameter naming the storage tier.
	ParameterTier = "tier"
	// ParameterFilesystemType is the StorageClass parameter naming the file system when the capability has none.
	ParameterFilesystemType = "fsType"

	// PublishContextDeviceSerial hands the virtio serial from ControllerPublish to the node.
	PublishContextDeviceSerial = "deviceSerial"

	// DefaultTier is used when a StorageClass names none.
	DefaultTier = "standard"
	// TierLocalNVMe is the Ankra Local tier: a volume on the compute node's own disks, pinned to it.
	TierLocalNVMe = "local-nvme"

	// DefaultMaximumVolumesPerNode is the API's MaximumStorageDevices (16) less the server's boot storage.
	DefaultMaximumVolumesPerNode = 15

	gibibyte = int64(1) << 30
	// maximumVolumeGibibytes mirrors domain.MaximumStorageGibibytes.
	maximumVolumeGibibytes = 4096
)

// Mode selects which services the process serves.
type Mode string

const (
	ModeController Mode = "controller"
	ModeNode       Mode = "node"
	ModeAll        Mode = "all"
)

// NodeIdentity is the server this node plugin runs on.
type NodeIdentity struct {
	ServerID string
	Zone     string
}

// NodeIdentityResolver finds the node's identity; it is called once, on the first NodeGetInfo or node RPC that
// needs it.
type NodeIdentityResolver func(ctx context.Context) (NodeIdentity, error)

// Options configure a Driver.
type Options struct {
	Mode    Mode
	Version string
	API     cloud.API
	// DefaultZone is the zone of volumes whose request carries no topology (the controller's own zone).
	DefaultZone string
	// ResolveNodeIdentity is required in node mode.
	ResolveNodeIdentity NodeIdentityResolver
	// Host is required in node mode.
	Host                  Host
	MaximumVolumesPerNode int64
	// OperationPollInterval is how often a running API operation is polled.
	OperationPollInterval time.Duration
	// OperationTimeout bounds how long one RPC waits for an API operation.
	OperationTimeout time.Duration
	// DeviceWaitTimeout bounds how long NodeStage waits for the hot-plugged disk to appear.
	DeviceWaitTimeout time.Duration
	Logger            *slog.Logger
}

// Driver serves the CSI services.
type Driver struct {
	csi.UnimplementedIdentityServer
	csi.UnimplementedControllerServer
	csi.UnimplementedNodeServer

	options      Options
	logger       *slog.Logger
	inFlight     sync.Map
	identityOnce sync.Mutex
	identity     *NodeIdentity
}

// New validates the options and builds a Driver.
func New(options Options) (*Driver, error) {
	if options.Mode == "" {
		options.Mode = ModeAll
	}
	switch options.Mode {
	case ModeController, ModeNode, ModeAll:
	default:
		return nil, fmt.Errorf("unknown mode %q", options.Mode)
	}
	if options.API == nil {
		return nil, errors.New("an API client is required")
	}
	if options.servesNode() {
		if options.Host == nil {
			return nil, errors.New("node mode needs a Host")
		}
		if options.ResolveNodeIdentity == nil {
			return nil, errors.New("node mode needs a node identity resolver")
		}
	}
	if options.MaximumVolumesPerNode <= 0 {
		options.MaximumVolumesPerNode = DefaultMaximumVolumesPerNode
	}
	if options.OperationPollInterval <= 0 {
		options.OperationPollInterval = 2 * time.Second
	}
	if options.OperationTimeout <= 0 {
		options.OperationTimeout = 5 * time.Minute
	}
	if options.DeviceWaitTimeout <= 0 {
		options.DeviceWaitTimeout = 60 * time.Second
	}
	if options.Version == "" {
		options.Version = "dev"
	}
	logger := options.Logger
	if logger == nil {
		logger = slog.New(slog.NewTextHandler(os.Stderr, nil))
	}
	return &Driver{options: options, logger: logger}, nil
}

func (options Options) servesController() bool {
	return options.Mode == ModeController || options.Mode == ModeAll
}

func (options Options) servesNode() bool {
	return options.Mode == ModeNode || options.Mode == ModeAll
}

// Register adds the services of the driver's mode to a gRPC server.
func (driver *Driver) Register(server *grpc.Server) {
	csi.RegisterIdentityServer(server, driver)
	if driver.options.servesController() {
		csi.RegisterControllerServer(server, driver)
	}
	if driver.options.servesNode() {
		csi.RegisterNodeServer(server, driver)
	}
}

// Listen opens the CSI endpoint, `unix:///csi/csi.sock` or `tcp://127.0.0.1:10000`, removing a stale socket.
func Listen(endpoint string) (net.Listener, error) {
	parsed, parseError := url.Parse(endpoint)
	if parseError != nil {
		return nil, fmt.Errorf("parse endpoint %q: %w", endpoint, parseError)
	}
	switch parsed.Scheme {
	case "unix":
		path := parsed.Path
		if path == "" {
			path = parsed.Host
		}
		if mkdirError := os.MkdirAll(filepath.Dir(path), 0o750); mkdirError != nil {
			return nil, mkdirError
		}
		if removeError := os.Remove(path); removeError != nil && !errors.Is(removeError, os.ErrNotExist) {
			return nil, fmt.Errorf("remove the stale socket %s: %w", path, removeError)
		}
		return net.Listen("unix", path)
	case "tcp":
		return net.Listen("tcp", parsed.Host)
	}
	return nil, fmt.Errorf("endpoint %q must be unix:// or tcp://", endpoint)
}

// NewServer builds the gRPC server with request logging and registers the driver on it.
func (driver *Driver) NewServer() *grpc.Server {
	server := grpc.NewServer(grpc.UnaryInterceptor(driver.logCall))
	driver.Register(server)
	return server
}

func (driver *Driver) logCall(ctx context.Context, request any, info *grpc.UnaryServerInfo, handler grpc.UnaryHandler) (any, error) {
	started := time.Now()
	response, callError := handler(ctx, request)
	method := info.FullMethod[strings.LastIndex(info.FullMethod, "/")+1:]
	if callError != nil {
		driver.logger.Warn("csi call failed", "method", method, "duration", time.Since(started), "error", callError)
	} else if method != "Probe" && method != "NodeGetCapabilities" && method != "NodeGetVolumeStats" {
		driver.logger.Info("csi call", "method", method, "duration", time.Since(started))
	}
	return response, callError
}

// lock serialises the operations on one volume or name. It never blocks: CSI asks a plugin to answer ABORTED while
// an operation on the same volume is still in progress, and the sidecars retry.
func (driver *Driver) lock(key string) (func(), bool) {
	if _, isHeld := driver.inFlight.LoadOrStore(key, struct{}{}); isHeld {
		return nil, false
	}
	return func() { driver.inFlight.Delete(key) }, true
}

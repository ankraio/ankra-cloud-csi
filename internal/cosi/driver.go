// Package cosi is the COSI driver cosi.ankra.cloud: the identity and provisioner services of the Container Object
// Storage Interface (cosi.v1alpha1) over Ankra Cloud object storage. The Ankra Cloud API enables object storage in a
// zone and hands out the account's one S3 key there; buckets are created and deleted with the S3 API under that key.
package cosi

import (
	"context"
	"errors"
	"log/slog"
	"os"
	"strings"
	"sync"
	"time"

	"google.golang.org/grpc"
	cosiapi "sigs.k8s.io/container-object-storage-interface/proto"

	"github.com/ankraio/ankra-cloud-csi/internal/cloud"
)

const (
	// Name is the COSI driver name, the driverName of BucketClasses and BucketAccessClasses.
	Name = "cosi.ankra.cloud"

	// ParameterZone is the BucketClass parameter naming the zone the bucket is created in. Required.
	ParameterZone = "zone"
	// ParameterBucketNamePrefix is the optional BucketClass parameter that starts every bucket name of the class,
	// after the account's own bucket prefix.
	ParameterBucketNamePrefix = "bucketNamePrefix"

	// Region is the region name handed to clients. The endpoints accept SigV4 signed with any region name.
	Region = "us-east-1"

	// DefaultEndpoint is where the driver listens when neither -endpoint nor COSI_ENDPOINT says otherwise; the
	// provisioner sidecar's default.
	DefaultEndpoint = "unix:///var/lib/cosi/cosi.sock"
)

// ObjectStorageAPI is the slice of the Ankra Cloud API the driver calls. Errors are *ankraapi.Error, so
// cloud.StatusCode tells them apart. cloud.Client implements it.
type ObjectStorageAPI interface {
	// EnableObjectStorage enables object storage in the zone, idempotently, and returns it with the account's key.
	EnableObjectStorage(ctx context.Context, zone string) (cloud.ObjectStorage, cloud.ObjectStorageKey, error)
	// GetObjectStorage reads the zone's object storage; 404 when the account has not enabled it there.
	GetObjectStorage(ctx context.Context, zone string) (cloud.ObjectStorage, error)
	// GetObjectStorageKey reads the account's key in the zone.
	GetObjectStorageKey(ctx context.Context, zone string) (cloud.ObjectStorageKey, error)
}

var _ ObjectStorageAPI = (*cloud.Client)(nil)

// Buckets is the S3 bucket API of one zone, signed with the account's key. Errors wrap ErrBucketAlreadyOwned,
// ErrBucketAlreadyExists, ErrBucketNotFound, ErrBucketNotEmpty or ErrAccessDenied when S3 answered one of those.
type Buckets interface {
	CreateBucket(ctx context.Context, name string) error
	// DeleteBucket deletes an empty bucket; S3 refuses a bucket that still holds objects.
	DeleteBucket(ctx context.Context, name string) error
	// HeadBucket succeeds when the bucket exists and the key may use it.
	HeadBucket(ctx context.Context, name string) error
}

// BucketsFactory connects to a zone's S3 endpoint with the account's key.
type BucketsFactory func(storage cloud.ObjectStorage, key cloud.ObjectStorageKey) (Buckets, error)

// S3 answers the driver tells apart.
var (
	ErrBucketAlreadyOwned  = errors.New("the bucket already exists and is owned by this account")
	ErrBucketAlreadyExists = errors.New("the bucket name is taken")
	ErrBucketNotFound      = errors.New("the bucket does not exist")
	ErrBucketNotEmpty      = errors.New("the bucket is not empty")
	ErrAccessDenied        = errors.New("the S3 key may not do this")
)

// Options configure a Driver.
type Options struct {
	API ObjectStorageAPI
	// Buckets connects to a zone's S3 endpoint; NewS3Buckets when nil.
	Buckets BucketsFactory
	Logger  *slog.Logger
}

// Driver serves the COSI identity and provisioner services.
type Driver struct {
	cosiapi.UnimplementedIdentityServer
	cosiapi.UnimplementedProvisionerServer

	options  Options
	logger   *slog.Logger
	inFlight sync.Map
}

// New validates the options and builds a Driver.
func New(options Options) (*Driver, error) {
	if options.API == nil {
		return nil, errors.New("an API client is required")
	}
	if options.Buckets == nil {
		options.Buckets = NewS3Buckets
	}
	logger := options.Logger
	if logger == nil {
		logger = slog.New(slog.NewTextHandler(os.Stderr, nil))
	}
	return &Driver{options: options, logger: logger}, nil
}

// Register adds the identity and provisioner services to a gRPC server.
func (driver *Driver) Register(server *grpc.Server) {
	cosiapi.RegisterIdentityServer(server, driver)
	cosiapi.RegisterProvisionerServer(server, driver)
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
		driver.logger.Warn("cosi call failed", "method", method, "duration", time.Since(started), "error", callError)
	} else if method != "DriverGetInfo" {
		driver.logger.Info("cosi call", "method", method, "duration", time.Since(started))
	}
	return response, callError
}

// lock serialises the calls on one bucket. It never blocks: a second call while one is in progress answers ABORTED
// and the sidecar retries it.
func (driver *Driver) lock(key string) (func(), bool) {
	if _, isHeld := driver.inFlight.LoadOrStore(key, struct{}{}); isHeld {
		return nil, false
	}
	return func() { driver.inFlight.Delete(key) }, true
}

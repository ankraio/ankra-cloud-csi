package cosi

import (
	"context"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"sync"
	"testing"

	"github.com/ankraio/ankra-cloud-csi/internal/cloud"
)

const (
	testZone            = "fsn1"
	testEndpoint        = "https://s3.fsn1.example.test"
	testAccessKeyID     = "AKIDEXAMPLE"
	testSecretAccessKey = "secret-example"
)

// fakeAPI is an in-memory ObjectStorageAPI: object storage is enabled per zone by EnableObjectStorage, and every call
// can be made to fail with an API status.
type fakeAPI struct {
	mutex sync.Mutex
	// bucketPrefixes is the account bucket prefix a zone gets once enabled (single-node zones).
	bucketPrefixes map[string]string
	enabled        map[string]cloud.ObjectStorage
	failures       map[string]int
	calls          map[string]int
}

func newFakeAPI() *fakeAPI {
	return &fakeAPI{bucketPrefixes: map[string]string{}, enabled: map[string]cloud.ObjectStorage{},
		failures: map[string]int{}, calls: map[string]int{}}
}

func (api *fakeAPI) record(method string) error {
	api.calls[method]++
	if statusCode, isFailing := api.failures[method]; isFailing {
		return cloud.NewError(statusCode, "injected failure")
	}
	return nil
}

func (api *fakeAPI) key() cloud.ObjectStorageKey {
	return cloud.ObjectStorageKey{AccessKeyID: testAccessKeyID, SecretAccessKey: testSecretAccessKey}
}

func (api *fakeAPI) EnableObjectStorage(_ context.Context, zone string) (cloud.ObjectStorage, cloud.ObjectStorageKey, error) {
	api.mutex.Lock()
	defer api.mutex.Unlock()
	if failure := api.record("EnableObjectStorage"); failure != nil {
		return cloud.ObjectStorage{}, cloud.ObjectStorageKey{}, failure
	}
	storage, isEnabled := api.enabled[zone]
	if !isEnabled {
		storage = cloud.ObjectStorage{Zone: zone, Endpoint: testEndpoint, Backend: cloud.ObjectStorageBackendReplicated,
			Durability: "replicated", QuotaBytes: 1 << 40}
		if prefix, isSingleNode := api.bucketPrefixes[zone]; isSingleNode {
			storage.Backend = cloud.ObjectStorageBackendSingleNode
			storage.BucketPrefix = prefix
		}
		api.enabled[zone] = storage
	}
	return storage, api.key(), nil
}

func (api *fakeAPI) GetObjectStorage(_ context.Context, zone string) (cloud.ObjectStorage, error) {
	api.mutex.Lock()
	defer api.mutex.Unlock()
	if failure := api.record("GetObjectStorage"); failure != nil {
		return cloud.ObjectStorage{}, failure
	}
	storage, isEnabled := api.enabled[zone]
	if !isEnabled {
		return cloud.ObjectStorage{}, cloud.NewError(http.StatusNotFound, fmt.Sprintf("object storage is not enabled in zone %s", zone))
	}
	return storage, nil
}

func (api *fakeAPI) GetObjectStorageKey(_ context.Context, zone string) (cloud.ObjectStorageKey, error) {
	api.mutex.Lock()
	defer api.mutex.Unlock()
	if failure := api.record("GetObjectStorageKey"); failure != nil {
		return cloud.ObjectStorageKey{}, failure
	}
	if _, isEnabled := api.enabled[zone]; !isEnabled {
		return cloud.ObjectStorageKey{}, cloud.NewError(http.StatusNotFound, "object storage is not enabled")
	}
	return api.key(), nil
}

// fakeBuckets is an in-memory S3 server shared by every zone: buckets with their object counts, and bucket names
// other accounts hold.
type fakeBuckets struct {
	mutex   sync.Mutex
	objects map[string]int
	// foreign are bucket names another account owns.
	foreign map[string]bool
	// answerExistsToOwner makes CreateBucket answer BucketAlreadyExists, not BucketAlreadyOwnedByYou, for an own bucket.
	answerExistsToOwner bool
	connections         []cloud.ObjectStorage
	created             []string
	deleted             []string
}

func newFakeBuckets() *fakeBuckets {
	return &fakeBuckets{objects: map[string]int{}, foreign: map[string]bool{}}
}

func (buckets *fakeBuckets) factory(storage cloud.ObjectStorage, key cloud.ObjectStorageKey) (Buckets, error) {
	buckets.mutex.Lock()
	defer buckets.mutex.Unlock()
	if key.AccessKeyID != testAccessKeyID || key.SecretAccessKey != testSecretAccessKey {
		return nil, fmt.Errorf("unexpected key %+v", key)
	}
	buckets.connections = append(buckets.connections, storage)
	return buckets, nil
}

func (buckets *fakeBuckets) CreateBucket(_ context.Context, name string) error {
	buckets.mutex.Lock()
	defer buckets.mutex.Unlock()
	if buckets.foreign[name] {
		return fmt.Errorf("%w: BucketAlreadyExists", ErrBucketAlreadyExists)
	}
	if _, exists := buckets.objects[name]; exists {
		if buckets.answerExistsToOwner {
			return fmt.Errorf("%w: BucketAlreadyExists", ErrBucketAlreadyExists)
		}
		return fmt.Errorf("%w: BucketAlreadyOwnedByYou", ErrBucketAlreadyOwned)
	}
	buckets.objects[name] = 0
	buckets.created = append(buckets.created, name)
	return nil
}

func (buckets *fakeBuckets) DeleteBucket(_ context.Context, name string) error {
	buckets.mutex.Lock()
	defer buckets.mutex.Unlock()
	count, exists := buckets.objects[name]
	if !exists {
		return fmt.Errorf("%w: NoSuchBucket", ErrBucketNotFound)
	}
	if count > 0 {
		return fmt.Errorf("%w: BucketNotEmpty", ErrBucketNotEmpty)
	}
	delete(buckets.objects, name)
	buckets.deleted = append(buckets.deleted, name)
	return nil
}

func (buckets *fakeBuckets) HeadBucket(_ context.Context, name string) error {
	buckets.mutex.Lock()
	defer buckets.mutex.Unlock()
	if buckets.foreign[name] {
		return fmt.Errorf("%w: Forbidden", ErrAccessDenied)
	}
	if _, exists := buckets.objects[name]; !exists {
		return fmt.Errorf("%w: NotFound", ErrBucketNotFound)
	}
	return nil
}

func newTestDriver(t *testing.T, api *fakeAPI, buckets *fakeBuckets) *Driver {
	t.Helper()
	cosiDriver, driverError := New(Options{API: api, Buckets: buckets.factory, Logger: slog.New(slog.NewTextHandler(io.Discard, nil))})
	if driverError != nil {
		t.Fatalf("New: %v", driverError)
	}
	return cosiDriver
}

package cosi

import (
	"context"
	"errors"
	"fmt"
	"sort"
	"strings"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	cosiapi "sigs.k8s.io/container-object-storage-interface/proto"

	"github.com/ankraio/ankra-cloud-csi/internal/cloud"
)

// Keys of the S3 credential set the provisioner sidecar writes into a BucketAccess's Secret.
const (
	credentialsS3              = "s3"
	credentialsAccessKeyID     = "accessKeyID"
	credentialsAccessSecretKey = "accessSecretKey"
	credentialsEndpoint        = "endpoint"
	credentialsRegion          = "region"
)

const (
	bucketLockPrefix             = "bucket/"
	bucketClassParametersAllowed = ParameterZone + ", " + ParameterBucketNamePrefix
)

// bucketParameters are a BucketClass's parameters.
type bucketParameters struct {
	zone        string
	classPrefix string
}

func parseBucketParameters(parameters map[string]string) (bucketParameters, error) {
	var parsed bucketParameters
	var unknown []string
	for key, value := range parameters {
		switch key {
		case ParameterZone:
			parsed.zone = strings.TrimSpace(value)
		case ParameterBucketNamePrefix:
			parsed.classPrefix = value
		default:
			unknown = append(unknown, key)
		}
	}
	if len(unknown) > 0 {
		sort.Strings(unknown)
		return bucketParameters{}, fmt.Errorf("unknown BucketClass parameters %s; %s takes %s",
			strings.Join(unknown, ", "), Name, bucketClassParametersAllowed)
	}
	if parsed.zone == "" {
		return bucketParameters{}, fmt.Errorf("the BucketClass parameter %s is required", ParameterZone)
	}
	if strings.Contains(parsed.zone, "/") {
		return bucketParameters{}, fmt.Errorf("zone %q is not a zone name", parsed.zone)
	}
	if parsed.classPrefix != "" {
		if prefixError := ValidateClassPrefix(parsed.classPrefix); prefixError != nil {
			return bucketParameters{}, prefixError
		}
	}
	return parsed, nil
}

// DriverCreateBucket enables object storage in the class's zone (idempotent), derives the bucket name from the COSI
// bucket name and creates the bucket with S3. A bucket this account already owns is success, so a retry finds the
// bucket it created. The bucket id is `<zone>/<bucket name>`.
func (driver *Driver) DriverCreateBucket(ctx context.Context, request *cosiapi.DriverCreateBucketRequest) (*cosiapi.DriverCreateBucketResponse, error) {
	cosiName := request.GetName()
	if cosiName == "" {
		return nil, status.Error(codes.InvalidArgument, "the bucket name is required")
	}
	parameters, parameterError := parseBucketParameters(request.GetParameters())
	if parameterError != nil {
		return nil, status.Error(codes.InvalidArgument, parameterError.Error())
	}
	release, isLocked := driver.lock(bucketLockPrefix + cosiName)
	if !isLocked {
		return nil, status.Errorf(codes.Aborted, "bucket %s is already being created or deleted", cosiName)
	}
	defer release()

	storage, key, enableError := driver.options.API.EnableObjectStorage(ctx, parameters.zone)
	if enableError != nil {
		return nil, apiStatus(enableError, "enable object storage in zone %s", parameters.zone)
	}
	bucketName, nameError := BucketName(storage.BucketPrefix, parameters.classPrefix, cosiName)
	if nameError != nil {
		return nil, status.Errorf(codes.FailedPrecondition, "zone %s: %v", parameters.zone, nameError)
	}
	buckets, connectError := driver.options.Buckets(storage, key)
	if connectError != nil {
		return nil, status.Errorf(codes.Internal, "connect to the S3 endpoint of zone %s: %v", parameters.zone, connectError)
	}
	createError := buckets.CreateBucket(ctx, bucketName)
	switch {
	case createError == nil, errors.Is(createError, ErrBucketAlreadyOwned):
	case errors.Is(createError, ErrBucketAlreadyExists):
		// Some S3 servers answer BucketAlreadyExists to the bucket's own owner too: a bucket the key can open is ours.
		if headError := buckets.HeadBucket(ctx, bucketName); headError != nil {
			return nil, status.Errorf(codes.FailedPrecondition,
				"the bucket name %s in zone %s belongs to someone else; set another %s on the BucketClass: %v",
				bucketName, parameters.zone, ParameterBucketNamePrefix, headError)
		}
	default:
		return nil, s3Status(createError, "create bucket %s in zone %s", bucketName, parameters.zone)
	}
	driver.logger.Info("bucket ready", "bucket", bucketName, "zone", parameters.zone, "cosi_name", cosiName)
	return &cosiapi.DriverCreateBucketResponse{
		BucketId: BucketID(parameters.zone, bucketName),
		BucketInfo: &cosiapi.Protocol{Type: &cosiapi.Protocol_S3{S3: &cosiapi.S3{
			Region: Region, SignatureVersion: cosiapi.S3SignatureVersion_S3V4}}},
	}, nil
}

// DriverDeleteBucket deletes the bucket with S3. It never deletes objects: a bucket that still holds any is refused
// with FAILED_PRECONDITION until it is emptied. A bucket that does not exist, or a zone where object storage was
// never enabled, is success.
func (driver *Driver) DriverDeleteBucket(ctx context.Context, request *cosiapi.DriverDeleteBucketRequest) (*cosiapi.DriverDeleteBucketResponse, error) {
	zone, bucketName, parseError := ParseBucketID(request.GetBucketId())
	if parseError != nil {
		return nil, status.Error(codes.InvalidArgument, parseError.Error())
	}
	release, isLocked := driver.lock(bucketLockPrefix + request.GetBucketId())
	if !isLocked {
		return nil, status.Errorf(codes.Aborted, "bucket %s is already being deleted", request.GetBucketId())
	}
	defer release()

	_, _, buckets, connectError := driver.connect(ctx, zone)
	if connectError != nil {
		if cloud.IsNotFound(connectError) {
			return &cosiapi.DriverDeleteBucketResponse{}, nil
		}
		return nil, apiStatus(connectError, "read the object storage of zone %s", zone)
	}
	deleteError := buckets.DeleteBucket(ctx, bucketName)
	switch {
	case deleteError == nil, errors.Is(deleteError, ErrBucketNotFound):
		driver.logger.Info("bucket deleted", "bucket", bucketName, "zone", zone)
		return &cosiapi.DriverDeleteBucketResponse{}, nil
	case errors.Is(deleteError, ErrBucketNotEmpty):
		return nil, status.Errorf(codes.FailedPrecondition,
			"bucket %s in zone %s still holds objects; %s never deletes objects: empty the bucket and the deletion is retried",
			bucketName, zone, Name)
	}
	return nil, s3Status(deleteError, "delete bucket %s in zone %s", bucketName, zone)
}

// DriverGrantBucketAccess answers the zone's account key as the S3 credentials of the access. Only the Key
// authentication type is supported. The Ankra Cloud API has one key per account and zone and no per-bucket keys, so
// every access granted in a zone gets the same key, and that key opens every bucket of the account in the zone.
func (driver *Driver) DriverGrantBucketAccess(ctx context.Context, request *cosiapi.DriverGrantBucketAccessRequest) (*cosiapi.DriverGrantBucketAccessResponse, error) {
	switch request.GetAuthenticationType() {
	case cosiapi.AuthenticationType_Key:
	case cosiapi.AuthenticationType_IAM:
		return nil, status.Errorf(codes.Unimplemented,
			"%s supports authenticationType Key only: Ankra Cloud object storage has no IAM or workload identity", Name)
	default:
		return nil, status.Errorf(codes.InvalidArgument, "authentication type %s is not supported; use Key",
			request.GetAuthenticationType())
	}
	accountName := request.GetName()
	if accountName == "" {
		return nil, status.Error(codes.InvalidArgument, "the access name is required")
	}
	zone, bucketName, parseError := ParseBucketID(request.GetBucketId())
	if parseError != nil {
		return nil, status.Error(codes.InvalidArgument, parseError.Error())
	}
	storage, key, buckets, connectError := driver.connect(ctx, zone)
	if connectError != nil {
		return nil, apiStatus(connectError, "read the object storage of zone %s", zone)
	}
	if headError := buckets.HeadBucket(ctx, bucketName); headError != nil {
		return nil, s3Status(headError, "read bucket %s in zone %s", bucketName, zone)
	}
	return &cosiapi.DriverGrantBucketAccessResponse{
		AccountId: accountName,
		Credentials: map[string]*cosiapi.CredentialDetails{
			credentialsS3: {Secrets: map[string]string{
				credentialsAccessKeyID:     key.AccessKeyID,
				credentialsAccessSecretKey: key.SecretAccessKey,
				credentialsEndpoint:        storage.Endpoint,
				credentialsRegion:          Region,
			}},
		},
	}, nil
}

// DriverRevokeBucketAccess succeeds without changing anything. The key a grant hands out is the account's one key in
// the zone, shared by every other access and by whatever else uses it; rotating it would break all of them. Rotate it
// deliberately with the Ankra Cloud API (rotate_object_storage_keys) when it must stop working.
func (driver *Driver) DriverRevokeBucketAccess(_ context.Context, request *cosiapi.DriverRevokeBucketAccessRequest) (*cosiapi.DriverRevokeBucketAccessResponse, error) {
	if _, _, parseError := ParseBucketID(request.GetBucketId()); parseError != nil {
		return nil, status.Error(codes.InvalidArgument, parseError.Error())
	}
	if request.GetAccountId() == "" {
		return nil, status.Error(codes.InvalidArgument, "the account id is required")
	}
	return &cosiapi.DriverRevokeBucketAccessResponse{}, nil
}

// connect reads the zone's object storage and key and connects to its S3 endpoint. A zone where the account never
// enabled object storage answers the API's 404.
func (driver *Driver) connect(ctx context.Context, zone string) (cloud.ObjectStorage, cloud.ObjectStorageKey, Buckets, error) {
	storage, getError := driver.options.API.GetObjectStorage(ctx, zone)
	if getError != nil {
		return cloud.ObjectStorage{}, cloud.ObjectStorageKey{}, nil, getError
	}
	key, keyError := driver.options.API.GetObjectStorageKey(ctx, zone)
	if keyError != nil {
		return cloud.ObjectStorage{}, cloud.ObjectStorageKey{}, nil, keyError
	}
	buckets, connectError := driver.options.Buckets(storage, key)
	if connectError != nil {
		return cloud.ObjectStorage{}, cloud.ObjectStorageKey{}, nil,
			status.Errorf(codes.Internal, "connect to the S3 endpoint of zone %s: %v", zone, connectError)
	}
	return storage, key, buckets, nil
}

package cloud

import (
	"context"
	"fmt"
	"net/http"

	"github.com/ankraio/ankra-cloud-csi/internal/ankraapi"
)

// Object storage backends.
const (
	// ObjectStorageBackendReplicated is replicated object storage on Ankra Storage.
	ObjectStorageBackendReplicated = "rgw"
	// ObjectStorageBackendSingleNode is the single-node S3 server of a zone without Ankra Storage; every bucket of the
	// account starts with the account's bucket prefix there.
	ObjectStorageBackendSingleNode = "ankra-s3-single"
)

// ObjectStorage is the token's account's S3-compatible object storage in one zone.
type ObjectStorage struct {
	Zone string
	// Endpoint is the S3 endpoint, an https URL served with path-style addressing and SigV4 under any region name.
	Endpoint string
	// CertificateAuthority is the PEM of the private CA that signed the endpoint's certificate; empty when a public CA
	// signed it.
	CertificateAuthority string
	// Backend is ObjectStorageBackendReplicated or ObjectStorageBackendSingleNode.
	Backend string
	// BucketPrefix starts the name of every bucket of the account when the zone requires it (single-node zones);
	// empty otherwise.
	BucketPrefix string
	Durability   string
	QuotaBytes   int64
	UsedBytes    int64
}

// ObjectStorageKey is the account's S3 key in a zone. There is one per account and zone; it opens every bucket of the
// account there.
type ObjectStorageKey struct {
	AccessKeyID     string
	SecretAccessKey string
}

// EnableObjectStorage enables object storage in the zone (idempotent: an enabled zone answers its current key) and
// returns it with the account's key (enable_object_storage).
func (client *Client) EnableObjectStorage(ctx context.Context, zone string) (ObjectStorage, ObjectStorageKey, error) {
	enabled, enableError := client.generated.EnableObjectStorage(ctx, ankraapi.EnableObjectStorageRequest{Zone: zone})
	if enableError != nil {
		return ObjectStorage{}, ObjectStorageKey{}, enableError
	}
	return objectStorageFromAPI(enabled.ObjectStorage), objectStorageKeyFromAPI(enabled.Credentials), nil
}

// GetObjectStorage reads the account's object storage in the zone (list_object_storage). A zone where the account has
// not enabled object storage answers a 404 API error.
func (client *Client) GetObjectStorage(ctx context.Context, zone string) (ObjectStorage, error) {
	listed, listError := client.generated.ListObjectStorage(ctx)
	if listError != nil {
		return ObjectStorage{}, listError
	}
	for _, storage := range listed.Items {
		if storage.Zone == zone {
			return objectStorageFromAPI(storage), nil
		}
	}
	return ObjectStorage{}, NewError(http.StatusNotFound, fmt.Sprintf("object storage is not enabled in zone %s", zone))
}

// GetObjectStorageKey reads the account's S3 key in the zone (get_object_storage_credentials; the API audits every
// read).
func (client *Client) GetObjectStorageKey(ctx context.Context, zone string) (ObjectStorageKey, error) {
	answer, getError := client.generated.GetObjectStorageCredentials(ctx, ankraapi.GetObjectStorageCredentialsParameters{Zone: zone})
	if getError != nil {
		return ObjectStorageKey{}, getError
	}
	return objectStorageKeyFromAPI(answer.Credentials), nil
}

func objectStorageFromAPI(storage ankraapi.ObjectStorage) ObjectStorage {
	converted := ObjectStorage{Zone: storage.Zone, Endpoint: storage.Endpoint, Backend: storage.Backend,
		Durability: storage.Durability, QuotaBytes: storage.QuotaBytes, UsedBytes: storage.UsedBytes}
	if storage.CertificateAuthority != nil {
		converted.CertificateAuthority = *storage.CertificateAuthority
	}
	if storage.BucketPrefix != nil {
		converted.BucketPrefix = *storage.BucketPrefix
	}
	return converted
}

func objectStorageKeyFromAPI(credentials ankraapi.ObjectStorageCredentials) ObjectStorageKey {
	return ObjectStorageKey{AccessKeyID: credentials.AccessKeyID, SecretAccessKey: credentials.SecretAccessKey}
}

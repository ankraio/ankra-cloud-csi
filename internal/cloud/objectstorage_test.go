package cloud

import (
	"context"
	"testing"
)

const objectStorageJSON = `{"id":"os-1","zone":"fsn1","endpoint":"https://s3.fsn1.example.test","certificate_authority":"-----BEGIN CERTIFICATE-----\nAA\n-----END CERTIFICATE-----\n",
"backend":"ankra-s3-single","bucket_prefix":"a1b2c3-","durability":"single_copy","quota_bytes":1099511627776,"used_bytes":42,
"object_count":1,"access_key_id":"AKIDEXAMPLE","created_at":"2026-11-01T12:00:00Z","usage_measured_at":null}`

const objectStorageKeyJSON = `{"access_key_id":"AKIDEXAMPLE","secret_access_key":"secret-example"}`

func TestEnableObjectStorageSendsTheZoneAndReturnsTheKey(t *testing.T) {
	client, calls := newAPIServer(t, map[string]string{
		"POST /v1/object-storage": `{"object_storage":` + objectStorageJSON + `,"credentials":` + objectStorageKeyJSON + `}`,
	})
	storage, key, enableError := client.EnableObjectStorage(context.Background(), "fsn1")
	if enableError != nil {
		t.Fatalf("EnableObjectStorage: %v", enableError)
	}
	if storage.Zone != "fsn1" || storage.Endpoint != "https://s3.fsn1.example.test" || storage.Backend != ObjectStorageBackendSingleNode ||
		storage.BucketPrefix != "a1b2c3-" || storage.CertificateAuthority == "" || storage.QuotaBytes != 1099511627776 || storage.UsedBytes != 42 {
		t.Fatalf("object storage %+v", storage)
	}
	if key.AccessKeyID != "AKIDEXAMPLE" || key.SecretAccessKey != "secret-example" {
		t.Fatalf("key %+v", key)
	}
	recorded := *calls
	if len(recorded) != 1 || recorded[0].body["zone"] != "fsn1" || recorded[0].authorization != "Bearer act_test" {
		t.Fatalf("calls %+v", recorded)
	}
}

func TestGetObjectStorageFindsTheZoneOrAnswersNotFound(t *testing.T) {
	client, _ := newAPIServer(t, map[string]string{
		"GET /v1/object-storage": `{"items":[` + objectStorageJSON + `],"next_cursor":null}`,
	})
	storage, getError := client.GetObjectStorage(context.Background(), "fsn1")
	if getError != nil || storage.Endpoint != "https://s3.fsn1.example.test" {
		t.Fatalf("GetObjectStorage: %+v %v", storage, getError)
	}
	if _, missingError := client.GetObjectStorage(context.Background(), "zone-b"); !IsNotFound(missingError) {
		t.Fatalf("a zone without object storage answered %v, want 404", missingError)
	}
}

func TestGetObjectStorageKeyReadsTheZoneCredentials(t *testing.T) {
	client, calls := newAPIServer(t, map[string]string{
		"GET /v1/object-storage/fsn1/credentials": `{"credentials":` + objectStorageKeyJSON + `}`,
	})
	key, getError := client.GetObjectStorageKey(context.Background(), "fsn1")
	if getError != nil || key.AccessKeyID != "AKIDEXAMPLE" || key.SecretAccessKey != "secret-example" {
		t.Fatalf("GetObjectStorageKey: %+v %v", key, getError)
	}
	if len(*calls) != 1 {
		t.Fatalf("calls %+v", *calls)
	}
}

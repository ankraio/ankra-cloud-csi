package cosi

import (
	"context"
	"encoding/pem"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"

	"github.com/ankraio/ankra-cloud-csi/internal/cloud"
)

// s3Server is a minimal path-style S3 server over TLS: buckets with object counts, names another account owns, and
// every request it saw.
type s3Server struct {
	mutex    sync.Mutex
	objects  map[string]int
	foreign  map[string]bool
	requests []*http.Request
}

func (server *s3Server) ServeHTTP(writer http.ResponseWriter, request *http.Request) {
	server.mutex.Lock()
	defer server.mutex.Unlock()
	server.requests = append(server.requests, request)
	bucket := strings.Trim(request.URL.Path, "/")
	answerError := func(statusCode int, code string) {
		writer.Header().Set("Content-Type", "application/xml")
		writer.WriteHeader(statusCode)
		if request.Method != http.MethodHead {
			_, _ = fmt.Fprintf(writer, `<?xml version="1.0" encoding="UTF-8"?><Error><Code>%s</Code><Message>%s</Message><BucketName>%s</BucketName><RequestId>tx1</RequestId></Error>`,
				code, code, bucket)
		}
	}
	count, exists := server.objects[bucket]
	switch request.Method {
	case http.MethodPut:
		switch {
		case server.foreign[bucket]:
			answerError(http.StatusConflict, "BucketAlreadyExists")
		case exists:
			answerError(http.StatusConflict, "BucketAlreadyOwnedByYou")
		default:
			server.objects[bucket] = 0
			writer.WriteHeader(http.StatusOK)
		}
	case http.MethodDelete:
		switch {
		case !exists:
			answerError(http.StatusNotFound, "NoSuchBucket")
		case count > 0:
			answerError(http.StatusConflict, "BucketNotEmpty")
		default:
			delete(server.objects, bucket)
			writer.WriteHeader(http.StatusNoContent)
		}
	case http.MethodHead:
		switch {
		case server.foreign[bucket]:
			answerError(http.StatusForbidden, "")
		case !exists:
			answerError(http.StatusNotFound, "")
		default:
			writer.WriteHeader(http.StatusOK)
		}
	default:
		answerError(http.StatusMethodNotAllowed, "MethodNotAllowed")
	}
}

// newS3Buckets serves an s3Server over TLS and connects to it the way the driver does, trusting the server's
// certificate as the zone's private certificate authority.
func newS3Buckets(t *testing.T) (Buckets, *s3Server) {
	t.Helper()
	fake := &s3Server{objects: map[string]int{}, foreign: map[string]bool{}}
	server := httptest.NewTLSServer(fake)
	t.Cleanup(server.Close)
	authority := pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: server.Certificate().Raw})
	buckets, connectError := NewS3Buckets(
		cloud.ObjectStorage{Zone: testZone, Endpoint: server.URL, CertificateAuthority: string(authority)},
		cloud.ObjectStorageKey{AccessKeyID: testAccessKeyID, SecretAccessKey: testSecretAccessKey})
	if connectError != nil {
		t.Fatalf("NewS3Buckets: %v", connectError)
	}
	return buckets, fake
}

func TestS3BucketsCreatesPathStyleWithSigV4AndTheZoneCertificateAuthority(t *testing.T) {
	buckets, server := newS3Buckets(t)
	if createError := buckets.CreateBucket(context.Background(), "a1b2c3-objects"); createError != nil {
		t.Fatalf("CreateBucket: %v", createError)
	}
	if len(server.requests) != 1 {
		t.Fatalf("requests %d", len(server.requests))
	}
	request := server.requests[0]
	if request.Method != http.MethodPut || request.URL.Path != "/a1b2c3-objects" {
		t.Fatalf("request %s %s, want a path-style PUT /a1b2c3-objects", request.Method, request.URL.Path)
	}
	authorization := request.Header.Get("Authorization")
	if !strings.HasPrefix(authorization, "AWS4-HMAC-SHA256 Credential="+testAccessKeyID+"/") || !strings.Contains(authorization, "/us-east-1/s3/") {
		t.Fatalf("authorization %q", authorization)
	}
}

func TestS3BucketsClassifiesTheAnswersTheDriverActsOn(t *testing.T) {
	buckets, server := newS3Buckets(t)
	ctx := context.Background()
	server.objects["owned"] = 0
	server.objects["full"] = 2
	server.foreign["taken"] = true

	checks := []struct {
		name   string
		call   func() error
		target error
	}{
		{"create an owned bucket", func() error { return buckets.CreateBucket(ctx, "owned") }, ErrBucketAlreadyOwned},
		{"create a taken name", func() error { return buckets.CreateBucket(ctx, "taken") }, ErrBucketAlreadyExists},
		{"delete a missing bucket", func() error { return buckets.DeleteBucket(ctx, "missing") }, ErrBucketNotFound},
		{"delete a bucket with objects", func() error { return buckets.DeleteBucket(ctx, "full") }, ErrBucketNotEmpty},
		{"head a missing bucket", func() error { return buckets.HeadBucket(ctx, "missing") }, ErrBucketNotFound},
		{"head another account's bucket", func() error { return buckets.HeadBucket(ctx, "taken") }, ErrAccessDenied},
	}
	for _, check := range checks {
		if callError := check.call(); !errors.Is(callError, check.target) {
			t.Errorf("%s: %v, want %v", check.name, callError, check.target)
		}
	}
	if headError := buckets.HeadBucket(ctx, "owned"); headError != nil {
		t.Fatalf("HeadBucket of an owned bucket: %v", headError)
	}
	if deleteError := buckets.DeleteBucket(ctx, "owned"); deleteError != nil {
		t.Fatalf("DeleteBucket of an empty bucket: %v", deleteError)
	}
	if server.objects["full"] != 2 {
		t.Fatalf("a refused delete changed the objects")
	}
}

func TestNewS3BucketsRefusesABrokenEndpointKeyOrCertificateAuthority(t *testing.T) {
	key := cloud.ObjectStorageKey{AccessKeyID: testAccessKeyID, SecretAccessKey: testSecretAccessKey}
	cases := map[string]struct {
		storage cloud.ObjectStorage
		key     cloud.ObjectStorageKey
	}{
		"no endpoint":           {cloud.ObjectStorage{Zone: testZone}, key},
		"not a URL":             {cloud.ObjectStorage{Zone: testZone, Endpoint: "s3.example.test"}, key},
		"no key":                {cloud.ObjectStorage{Zone: testZone, Endpoint: testEndpoint}, cloud.ObjectStorageKey{}},
		"no PEM in the CA":      {cloud.ObjectStorage{Zone: testZone, Endpoint: testEndpoint, CertificateAuthority: "not a certificate"}, key},
		"an endpoint with ftp:": {cloud.ObjectStorage{Zone: testZone, Endpoint: "ftp://s3.example.test"}, key},
	}
	for name, testCase := range cases {
		if _, connectError := NewS3Buckets(testCase.storage, testCase.key); connectError == nil {
			t.Errorf("%s: NewS3Buckets accepted it", name)
		}
	}
}

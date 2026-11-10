package cosi

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/s3"
	"github.com/aws/smithy-go"
	smithyhttp "github.com/aws/smithy-go/transport/http"

	"github.com/ankraio/ankra-cloud-csi/internal/cloud"
)

// s3Timeout bounds one S3 request.
const s3Timeout = 60 * time.Second

// S3Buckets is Buckets over a zone's S3 endpoint with the AWS SDK for Go v2: path-style addressing, SigV4 signed for
// Region, and the zone's private CA trusted next to the system roots when the API names one.
type S3Buckets struct {
	client *s3.Client
}

var _ Buckets = (*S3Buckets)(nil)

// NewS3Buckets connects to the zone's S3 endpoint with the account's key.
func NewS3Buckets(storage cloud.ObjectStorage, key cloud.ObjectStorageKey) (Buckets, error) {
	endpoint, parseError := url.Parse(storage.Endpoint)
	if parseError != nil || (endpoint.Scheme != "https" && endpoint.Scheme != "http") || endpoint.Host == "" {
		return nil, fmt.Errorf("the S3 endpoint %q of zone %s is not an http or https URL", storage.Endpoint, storage.Zone)
	}
	if key.AccessKeyID == "" || key.SecretAccessKey == "" {
		return nil, fmt.Errorf("the API answered no S3 key for zone %s", storage.Zone)
	}
	transport := http.DefaultTransport.(*http.Transport).Clone()
	if storage.CertificateAuthority != "" {
		roots, rootsError := x509.SystemCertPool()
		if rootsError != nil || roots == nil {
			roots = x509.NewCertPool()
		}
		if !roots.AppendCertsFromPEM([]byte(storage.CertificateAuthority)) {
			return nil, fmt.Errorf("the certificate authority of zone %s holds no PEM certificate", storage.Zone)
		}
		transport.TLSClientConfig = &tls.Config{RootCAs: roots, MinVersion: tls.VersionTLS12}
	}
	credentials := aws.Credentials{AccessKeyID: key.AccessKeyID, SecretAccessKey: key.SecretAccessKey, Source: "ankra-cloud-object-storage"}
	client := s3.New(s3.Options{
		BaseEndpoint: aws.String(storage.Endpoint),
		UsePathStyle: true,
		Region:       Region,
		Credentials: aws.CredentialsProviderFunc(func(context.Context) (aws.Credentials, error) {
			return credentials, nil
		}),
		HTTPClient: &http.Client{Timeout: s3Timeout, Transport: transport},
		// The default adds CRC checksums S3-compatible servers need not understand; bucket calls need none.
		RequestChecksumCalculation: aws.RequestChecksumCalculationWhenRequired,
		ResponseChecksumValidation: aws.ResponseChecksumValidationWhenRequired,
	})
	return &S3Buckets{client: client}, nil
}

// CreateBucket creates the bucket (CreateBucket, no location constraint).
func (buckets *S3Buckets) CreateBucket(ctx context.Context, name string) error {
	_, createError := buckets.client.CreateBucket(ctx, &s3.CreateBucketInput{Bucket: aws.String(name)})
	return classifyS3Error(createError)
}

// DeleteBucket deletes the bucket (DeleteBucket); S3 refuses one that holds objects.
func (buckets *S3Buckets) DeleteBucket(ctx context.Context, name string) error {
	_, deleteError := buckets.client.DeleteBucket(ctx, &s3.DeleteBucketInput{Bucket: aws.String(name)})
	return classifyS3Error(deleteError)
}

// HeadBucket checks that the bucket exists and the key may use it (HeadBucket).
func (buckets *S3Buckets) HeadBucket(ctx context.Context, name string) error {
	_, headError := buckets.client.HeadBucket(ctx, &s3.HeadBucketInput{Bucket: aws.String(name)})
	return classifyS3Error(headError)
}

// classifyS3Error wraps the S3 answers the driver acts on in their sentinel errors. HeadBucket answers carry no body,
// so the HTTP status decides those.
func classifyS3Error(callError error) error {
	if callError == nil {
		return nil
	}
	code := ""
	var apiError smithy.APIError
	if errors.As(callError, &apiError) {
		code = apiError.ErrorCode()
	}
	statusCode := 0
	var responseError *smithyhttp.ResponseError
	if errors.As(callError, &responseError) {
		statusCode = responseError.HTTPStatusCode()
	}
	switch {
	case code == "BucketAlreadyOwnedByYou":
		return fmt.Errorf("%w: %w", ErrBucketAlreadyOwned, callError)
	case code == "BucketAlreadyExists":
		return fmt.Errorf("%w: %w", ErrBucketAlreadyExists, callError)
	case code == "BucketNotEmpty":
		return fmt.Errorf("%w: %w", ErrBucketNotEmpty, callError)
	case code == "NoSuchBucket", code == "NotFound", statusCode == http.StatusNotFound:
		return fmt.Errorf("%w: %w", ErrBucketNotFound, callError)
	case code == "AccessDenied", code == "InvalidAccessKeyId", code == "SignatureDoesNotMatch", code == "Forbidden",
		statusCode == http.StatusForbidden:
		return fmt.Errorf("%w: %w", ErrAccessDenied, callError)
	}
	return callError
}

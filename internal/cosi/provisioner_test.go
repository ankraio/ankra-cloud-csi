package cosi

import (
	"context"
	"net/http"
	"testing"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	cosiapi "sigs.k8s.io/container-object-storage-interface/proto"
)

const testCOSIName = "objects-0f4c2a8e-1b2c-4d5e-8f90-123456789abc"

func expectCode(t *testing.T, callError error, code codes.Code) {
	t.Helper()
	if status.Code(callError) != code {
		t.Fatalf("code %s (%v), want %s", status.Code(callError), callError, code)
	}
}

func createBucketRequest(parameters map[string]string) *cosiapi.DriverCreateBucketRequest {
	return &cosiapi.DriverCreateBucketRequest{Name: testCOSIName, Parameters: parameters}
}

func TestDriverGetInfoNamesTheDriver(t *testing.T) {
	cosiDriver := newTestDriver(t, newFakeAPI(), newFakeBuckets())
	info, infoError := cosiDriver.DriverGetInfo(context.Background(), &cosiapi.DriverGetInfoRequest{})
	if infoError != nil || info.GetName() != "cosi.ankra.cloud" {
		t.Fatalf("DriverGetInfo = %v, %v", info, infoError)
	}
}

func TestCreateBucketEnablesObjectStorageAndCreatesTheBucket(t *testing.T) {
	api := newFakeAPI()
	buckets := newFakeBuckets()
	cosiDriver := newTestDriver(t, api, buckets)
	response, createError := cosiDriver.DriverCreateBucket(context.Background(), createBucketRequest(map[string]string{ParameterZone: testZone}))
	if createError != nil {
		t.Fatalf("DriverCreateBucket: %v", createError)
	}
	if api.calls["EnableObjectStorage"] != 1 {
		t.Fatalf("EnableObjectStorage called %d times", api.calls["EnableObjectStorage"])
	}
	if len(buckets.created) != 1 || buckets.created[0] != testCOSIName {
		t.Fatalf("created %v", buckets.created)
	}
	if response.GetBucketId() != testZone+"/"+testCOSIName {
		t.Fatalf("bucket id %q", response.GetBucketId())
	}
	s3Info := response.GetBucketInfo().GetS3()
	if s3Info == nil || s3Info.GetRegion() != Region || s3Info.GetSignatureVersion() != cosiapi.S3SignatureVersion_S3V4 {
		t.Fatalf("bucket info %v", response.GetBucketInfo())
	}
	if len(buckets.connections) != 1 || buckets.connections[0].Endpoint != testEndpoint {
		t.Fatalf("connections %+v", buckets.connections)
	}
}

func TestCreateBucketIsIdempotentWhenTheBucketIsAlreadyOwned(t *testing.T) {
	api := newFakeAPI()
	buckets := newFakeBuckets()
	cosiDriver := newTestDriver(t, api, buckets)
	request := createBucketRequest(map[string]string{ParameterZone: testZone})
	first, firstError := cosiDriver.DriverCreateBucket(context.Background(), request)
	second, secondError := cosiDriver.DriverCreateBucket(context.Background(), request)
	if firstError != nil || secondError != nil {
		t.Fatalf("DriverCreateBucket: %v %v", firstError, secondError)
	}
	if first.GetBucketId() != second.GetBucketId() || len(buckets.created) != 1 {
		t.Fatalf("ids %q %q, created %v", first.GetBucketId(), second.GetBucketId(), buckets.created)
	}
}

func TestCreateBucketTreatsAlreadyExistsForItsOwnBucketAsSuccess(t *testing.T) {
	buckets := newFakeBuckets()
	buckets.objects[testCOSIName] = 0
	buckets.answerExistsToOwner = true
	cosiDriver := newTestDriver(t, newFakeAPI(), buckets)
	if _, createError := cosiDriver.DriverCreateBucket(context.Background(), createBucketRequest(map[string]string{ParameterZone: testZone})); createError != nil {
		t.Fatalf("DriverCreateBucket: %v", createError)
	}
}

func TestCreateBucketRefusesANameAnotherAccountHolds(t *testing.T) {
	buckets := newFakeBuckets()
	buckets.foreign[testCOSIName] = true
	cosiDriver := newTestDriver(t, newFakeAPI(), buckets)
	_, createError := cosiDriver.DriverCreateBucket(context.Background(), createBucketRequest(map[string]string{ParameterZone: testZone}))
	expectCode(t, createError, codes.FailedPrecondition)
}

func TestCreateBucketPutsTheAccountPrefixFirstOnSingleNodeZones(t *testing.T) {
	api := newFakeAPI()
	api.bucketPrefixes["zone-b"] = "a1b2c3-"
	buckets := newFakeBuckets()
	cosiDriver := newTestDriver(t, api, buckets)
	response, createError := cosiDriver.DriverCreateBucket(context.Background(),
		createBucketRequest(map[string]string{ParameterZone: "zone-b", ParameterBucketNamePrefix: "team-a"}))
	if createError != nil {
		t.Fatalf("DriverCreateBucket: %v", createError)
	}
	want := "a1b2c3-team-a-" + testCOSIName
	if len(buckets.created) != 1 || buckets.created[0] != want || response.GetBucketId() != "zone-b/"+want {
		t.Fatalf("created %v, id %q", buckets.created, response.GetBucketId())
	}
}

func TestCreateBucketValidatesItsParameters(t *testing.T) {
	cases := map[string]map[string]string{
		"no zone":            nil,
		"empty zone":         {ParameterZone: " "},
		"zone with a slash":  {ParameterZone: "fsn1/x"},
		"unknown parameter":  {ParameterZone: testZone, "tier": "fast"},
		"invalid prefix":     {ParameterZone: testZone, ParameterBucketNamePrefix: "Team_A"},
		"prefix too long":    {ParameterZone: testZone, ParameterBucketNamePrefix: "a-very-long-prefix-for-buckets"},
		"prefix with a dash": {ParameterZone: testZone, ParameterBucketNamePrefix: "-team"},
	}
	for name, parameters := range cases {
		t.Run(name, func(t *testing.T) {
			api := newFakeAPI()
			cosiDriver := newTestDriver(t, api, newFakeBuckets())
			_, createError := cosiDriver.DriverCreateBucket(context.Background(), createBucketRequest(parameters))
			expectCode(t, createError, codes.InvalidArgument)
			if api.calls["EnableObjectStorage"] != 0 {
				t.Fatalf("the API was called for an invalid request")
			}
		})
	}
	cosiDriver := newTestDriver(t, newFakeAPI(), newFakeBuckets())
	_, unnamedError := cosiDriver.DriverCreateBucket(context.Background(),
		&cosiapi.DriverCreateBucketRequest{Parameters: map[string]string{ParameterZone: testZone}})
	expectCode(t, unnamedError, codes.InvalidArgument)
}

func TestCreateBucketMapsAPIErrors(t *testing.T) {
	cases := map[int]codes.Code{
		http.StatusPaymentRequired:    codes.FailedPrecondition,
		http.StatusForbidden:          codes.PermissionDenied,
		http.StatusServiceUnavailable: codes.Unavailable,
	}
	for statusCode, want := range cases {
		api := newFakeAPI()
		api.failures["EnableObjectStorage"] = statusCode
		cosiDriver := newTestDriver(t, api, newFakeBuckets())
		_, createError := cosiDriver.DriverCreateBucket(context.Background(), createBucketRequest(map[string]string{ParameterZone: testZone}))
		expectCode(t, createError, want)
	}
}

func TestCreateBucketAnswersAbortedWhileTheSameBucketIsInFlight(t *testing.T) {
	cosiDriver := newTestDriver(t, newFakeAPI(), newFakeBuckets())
	release, isLocked := cosiDriver.lock(bucketLockPrefix + testCOSIName)
	if !isLocked {
		t.Fatal("lock")
	}
	defer release()
	_, createError := cosiDriver.DriverCreateBucket(context.Background(), createBucketRequest(map[string]string{ParameterZone: testZone}))
	expectCode(t, createError, codes.Aborted)
}

func TestDeleteBucketDeletesAnEmptyBucket(t *testing.T) {
	api := newFakeAPI()
	buckets := newFakeBuckets()
	cosiDriver := newTestDriver(t, api, buckets)
	created, createError := cosiDriver.DriverCreateBucket(context.Background(), createBucketRequest(map[string]string{ParameterZone: testZone}))
	if createError != nil {
		t.Fatalf("DriverCreateBucket: %v", createError)
	}
	if _, deleteError := cosiDriver.DriverDeleteBucket(context.Background(),
		&cosiapi.DriverDeleteBucketRequest{BucketId: created.GetBucketId()}); deleteError != nil {
		t.Fatalf("DriverDeleteBucket: %v", deleteError)
	}
	if len(buckets.deleted) != 1 || buckets.deleted[0] != testCOSIName {
		t.Fatalf("deleted %v", buckets.deleted)
	}
	if api.calls["EnableObjectStorage"] != 1 {
		t.Fatalf("DriverDeleteBucket enabled object storage")
	}
}

func TestDeleteBucketTreatsAMissingBucketAsDeleted(t *testing.T) {
	api := newFakeAPI()
	cosiDriver := newTestDriver(t, api, newFakeBuckets())
	if _, _, enableError := api.EnableObjectStorage(context.Background(), testZone); enableError != nil {
		t.Fatal(enableError)
	}
	if _, deleteError := cosiDriver.DriverDeleteBucket(context.Background(),
		&cosiapi.DriverDeleteBucketRequest{BucketId: testZone + "/gone"}); deleteError != nil {
		t.Fatalf("DriverDeleteBucket of a missing bucket: %v", deleteError)
	}
}

func TestDeleteBucketTreatsAZoneWithoutObjectStorageAsDeleted(t *testing.T) {
	buckets := newFakeBuckets()
	cosiDriver := newTestDriver(t, newFakeAPI(), buckets)
	if _, deleteError := cosiDriver.DriverDeleteBucket(context.Background(),
		&cosiapi.DriverDeleteBucketRequest{BucketId: "zone-b/objects"}); deleteError != nil {
		t.Fatalf("DriverDeleteBucket: %v", deleteError)
	}
	if len(buckets.connections) != 0 {
		t.Fatalf("connected to S3 in a zone without object storage")
	}
}

func TestDeleteBucketRefusesABucketWithObjectsAndKeepsThem(t *testing.T) {
	api := newFakeAPI()
	buckets := newFakeBuckets()
	cosiDriver := newTestDriver(t, api, buckets)
	created, _ := cosiDriver.DriverCreateBucket(context.Background(), createBucketRequest(map[string]string{ParameterZone: testZone}))
	buckets.objects[testCOSIName] = 3
	_, deleteError := cosiDriver.DriverDeleteBucket(context.Background(), &cosiapi.DriverDeleteBucketRequest{BucketId: created.GetBucketId()})
	expectCode(t, deleteError, codes.FailedPrecondition)
	if buckets.objects[testCOSIName] != 3 || len(buckets.deleted) != 0 {
		t.Fatalf("objects %v, deleted %v", buckets.objects, buckets.deleted)
	}
}

func TestDeleteBucketValidatesTheBucketID(t *testing.T) {
	cosiDriver := newTestDriver(t, newFakeAPI(), newFakeBuckets())
	_, deleteError := cosiDriver.DriverDeleteBucket(context.Background(), &cosiapi.DriverDeleteBucketRequest{BucketId: "no-zone"})
	expectCode(t, deleteError, codes.InvalidArgument)
}

func TestDeleteBucketMapsAPIErrors(t *testing.T) {
	api := newFakeAPI()
	api.failures["GetObjectStorage"] = http.StatusUnauthorized
	cosiDriver := newTestDriver(t, api, newFakeBuckets())
	_, deleteError := cosiDriver.DriverDeleteBucket(context.Background(), &cosiapi.DriverDeleteBucketRequest{BucketId: testZone + "/objects"})
	expectCode(t, deleteError, codes.Unauthenticated)
}

func grantRequest(bucketID string, authenticationType cosiapi.AuthenticationType) *cosiapi.DriverGrantBucketAccessRequest {
	return &cosiapi.DriverGrantBucketAccessRequest{BucketId: bucketID, Name: "ba-6a1f0e2c", AuthenticationType: authenticationType}
}

func TestGrantBucketAccessReturnsTheZoneKeyInTheS3Format(t *testing.T) {
	api := newFakeAPI()
	buckets := newFakeBuckets()
	cosiDriver := newTestDriver(t, api, buckets)
	created, _ := cosiDriver.DriverCreateBucket(context.Background(), createBucketRequest(map[string]string{ParameterZone: testZone}))
	response, grantError := cosiDriver.DriverGrantBucketAccess(context.Background(), grantRequest(created.GetBucketId(), cosiapi.AuthenticationType_Key))
	if grantError != nil {
		t.Fatalf("DriverGrantBucketAccess: %v", grantError)
	}
	if response.GetAccountId() != "ba-6a1f0e2c" {
		t.Fatalf("account id %q", response.GetAccountId())
	}
	credentials := response.GetCredentials()
	if len(credentials) != 1 || credentials["s3"] == nil {
		t.Fatalf("credentials %v", credentials)
	}
	secrets := credentials["s3"].GetSecrets()
	if secrets["accessKeyID"] != testAccessKeyID || secrets["accessSecretKey"] != testSecretAccessKey ||
		secrets["endpoint"] != testEndpoint || secrets["region"] != "us-east-1" {
		t.Fatalf("secrets %v", secrets)
	}
}

func TestGrantBucketAccessRefusesIAM(t *testing.T) {
	api := newFakeAPI()
	cosiDriver := newTestDriver(t, api, newFakeBuckets())
	_, grantError := cosiDriver.DriverGrantBucketAccess(context.Background(), grantRequest(testZone+"/objects", cosiapi.AuthenticationType_IAM))
	expectCode(t, grantError, codes.Unimplemented)
	_, unknownError := cosiDriver.DriverGrantBucketAccess(context.Background(),
		grantRequest(testZone+"/objects", cosiapi.AuthenticationType_UnknownAuthenticationType))
	expectCode(t, unknownError, codes.InvalidArgument)
	if api.calls["GetObjectStorageKey"] != 0 {
		t.Fatalf("a refused grant read the key")
	}
}

func TestGrantBucketAccessNeedsAnExistingBucket(t *testing.T) {
	api := newFakeAPI()
	cosiDriver := newTestDriver(t, api, newFakeBuckets())
	_, notEnabledError := cosiDriver.DriverGrantBucketAccess(context.Background(), grantRequest(testZone+"/objects", cosiapi.AuthenticationType_Key))
	expectCode(t, notEnabledError, codes.NotFound)
	if _, _, enableError := api.EnableObjectStorage(context.Background(), testZone); enableError != nil {
		t.Fatal(enableError)
	}
	_, missingError := cosiDriver.DriverGrantBucketAccess(context.Background(), grantRequest(testZone+"/objects", cosiapi.AuthenticationType_Key))
	expectCode(t, missingError, codes.NotFound)
}

func TestGrantBucketAccessValidatesItsRequest(t *testing.T) {
	cosiDriver := newTestDriver(t, newFakeAPI(), newFakeBuckets())
	_, badIDError := cosiDriver.DriverGrantBucketAccess(context.Background(), grantRequest("objects", cosiapi.AuthenticationType_Key))
	expectCode(t, badIDError, codes.InvalidArgument)
	unnamed := grantRequest(testZone+"/objects", cosiapi.AuthenticationType_Key)
	unnamed.Name = ""
	_, unnamedError := cosiDriver.DriverGrantBucketAccess(context.Background(), unnamed)
	expectCode(t, unnamedError, codes.InvalidArgument)
}

func TestRevokeBucketAccessChangesNothing(t *testing.T) {
	api := newFakeAPI()
	buckets := newFakeBuckets()
	cosiDriver := newTestDriver(t, api, buckets)
	if _, revokeError := cosiDriver.DriverRevokeBucketAccess(context.Background(),
		&cosiapi.DriverRevokeBucketAccessRequest{BucketId: testZone + "/objects", AccountId: "ba-6a1f0e2c"}); revokeError != nil {
		t.Fatalf("DriverRevokeBucketAccess: %v", revokeError)
	}
	if len(api.calls) != 0 || len(buckets.connections) != 0 {
		t.Fatalf("revoke called the API %v or S3 %v", api.calls, buckets.connections)
	}
	_, missingAccountError := cosiDriver.DriverRevokeBucketAccess(context.Background(),
		&cosiapi.DriverRevokeBucketAccessRequest{BucketId: testZone + "/objects"})
	expectCode(t, missingAccountError, codes.InvalidArgument)
	_, badIDError := cosiDriver.DriverRevokeBucketAccess(context.Background(),
		&cosiapi.DriverRevokeBucketAccessRequest{BucketId: "objects", AccountId: "ba-6a1f0e2c"})
	expectCode(t, badIDError, codes.InvalidArgument)
}

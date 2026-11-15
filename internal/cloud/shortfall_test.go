package cloud

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"
)

// newRefusingClient is a Client whose API answers every request with the status and the problem document given.
func newRefusingClient(t *testing.T, statusCode int, problem string) *Client {
	t.Helper()
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		_, _ = io.Copy(io.Discard, request.Body)
		writer.Header().Set("Content-Type", "application/problem+json")
		writer.WriteHeader(statusCode)
		_, _ = io.WriteString(writer, problem)
	}))
	t.Cleanup(server.Close)
	client, clientError := NewClient(Configuration{Endpoint: server.URL, Token: "act_test", Timeout: 5 * time.Second})
	if clientError != nil {
		t.Fatalf("NewClient: %v", clientError)
	}
	return client
}

// problemDocument is the body the API answers a refusal with; reason is left out when it is empty.
func problemDocument(statusCode int, detail string, reason string) string {
	document := fmt.Sprintf(`{"detail":%q,"status":%d,"title":%q,"type":"about:blank"`, detail, statusCode, http.StatusText(statusCode))
	if reason != "" {
		document += fmt.Sprintf(`,"reason":%q`, reason)
	}
	return document + "}"
}

// The answers are the API's own bodies, sent over HTTP and read by the client the driver runs with: a full pool is
// recognised by the reason member, whatever the detail says.
func TestIsStorageUnavailableReadsTheReasonOfTheAnswer(t *testing.T) {
	const olderDetail = "the storage backend is not available in this zone: the node's Ankra Local pool takes no new volumes"
	cases := []struct {
		name          string
		statusCode    int
		problem       string
		isUnavailable bool
	}{
		{"the node's pool or the tier takes no new storage", http.StatusConflict,
			problemDocument(http.StatusConflict, StorageShortfallDetail, ShortfallReasonNotOffered), true},
		{"the zone has no room right now", http.StatusServiceUnavailable,
			problemDocument(http.StatusServiceUnavailable, StorageShortfallDetail, ShortfallReasonCapacity), true},
		{"the reason decides when the detail is reworded again", http.StatusConflict,
			problemDocument(http.StatusConflict, "No room for this storage here.", ShortfallReasonNotOffered), true},
		{"an API without a reason member, by its wording", http.StatusConflict,
			problemDocument(http.StatusConflict, olderDetail, ""), true},
		{"today's wording without a reason member", http.StatusConflict,
			problemDocument(http.StatusConflict, StorageShortfallDetail, ""), false},
		{"the older wording under another status", http.StatusServiceUnavailable,
			problemDocument(http.StatusServiceUnavailable, olderDetail, ""), false},
		{"another conflict", http.StatusConflict,
			problemDocument(http.StatusConflict, "the storage is attached", ""), false},
		{"a quota refusal", http.StatusUnprocessableEntity,
			problemDocument(http.StatusUnprocessableEntity, "the account's storage limit is reached", "quota_exceeded"), false},
		{"a reason under a status the API does not pair it with", http.StatusUnprocessableEntity,
			problemDocument(http.StatusUnprocessableEntity, StorageShortfallDetail, ShortfallReasonNotOffered), false},
		{"unavailable without a problem document", http.StatusServiceUnavailable, "upstream connect error", false},
		{"a reason that is not a string", http.StatusConflict,
			`{"detail":"refused","status":409,"title":"Conflict","type":"about:blank","reason":{"kind":"not_available_in_zone"}}`, false},
	}
	for _, testCase := range cases {
		t.Run(testCase.name, func(t *testing.T) {
			client := newRefusingClient(t, testCase.statusCode, testCase.problem)
			_, _, createError := client.CreateVolume(context.Background(),
				CreateVolumeRequest{Zone: "fsn1", Title: "pvc-1", Tier: "local-nvme", SizeGibibytes: 10, PlacementServerID: "srv-1"})
			if createError == nil {
				t.Fatal("the refused create returned no error")
			}
			if status := StatusCode(createError); status != testCase.statusCode {
				t.Fatalf("status %d, want %d: %v", status, testCase.statusCode, createError)
			}
			if isUnavailable := IsStorageUnavailable(createError); isUnavailable != testCase.isUnavailable {
				t.Fatalf("IsStorageUnavailable = %t, want %t for %s", isUnavailable, testCase.isUnavailable, testCase.problem)
			}
		})
	}
}

func TestIsStorageUnavailableIsFalseForOtherErrors(t *testing.T) {
	if IsStorageUnavailable(nil) {
		t.Fatal("no error read as a storage shortfall")
	}
	if IsStorageUnavailable(context.DeadlineExceeded) {
		t.Fatal("a deadline read as a storage shortfall")
	}
	if IsStorageUnavailable(fmt.Errorf("create: %w", NewError(http.StatusConflict, "the storage is attached"))) {
		t.Fatal("another conflict read as a storage shortfall")
	}
}

// NewShortfallError is what the fakes answer with, so it has to read the way the API's answer does.
func TestNewShortfallErrorReadsAsTheAPIAnswer(t *testing.T) {
	wrapped := fmt.Errorf("create: %w", NewShortfallError(http.StatusConflict, StorageShortfallDetail, ShortfallReasonNotOffered))
	if !IsStorageUnavailable(wrapped) {
		t.Fatal("the fake's 409 shortfall is not read as one")
	}
	if !IsStorageUnavailable(NewShortfallError(http.StatusServiceUnavailable, StorageShortfallDetail, ShortfallReasonCapacity)) {
		t.Fatal("the fake's 503 shortfall is not read as one")
	}
	if IsStorageUnavailable(NewError(http.StatusConflict, StorageShortfallDetail)) {
		t.Fatal("the wording alone, without a reason, was read as a shortfall")
	}
}

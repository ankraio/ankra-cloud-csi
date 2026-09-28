package metadata

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

func TestServerIDFallsBackToTheSecondEndpoint(t *testing.T) {
	var requestedPath string
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		requestedPath = request.URL.Path
		_, _ = io.WriteString(writer, "01a0d058-e588-7fff-8000-000000000001\n")
	}))
	defer server.Close()
	client := &Client{Endpoints: []string{"http://127.0.0.1:1", server.URL}, HTTPClient: &http.Client{Timeout: time.Second}}
	serverID, readError := client.ServerID(context.Background())
	if readError != nil {
		t.Fatalf("ServerID: %v", readError)
	}
	if serverID != "01a0d058-e588-7fff-8000-000000000001" {
		t.Fatalf("server id %q", serverID)
	}
	if requestedPath != ServerIDPath {
		t.Fatalf("path %q, want %q", requestedPath, ServerIDPath)
	}
}

func TestServerIDReportsEveryEndpoint(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, _ *http.Request) {
		http.Error(writer, "the metadata service is turned off for this server", http.StatusNotFound)
	}))
	defer server.Close()
	client := &Client{Endpoints: []string{server.URL}, HTTPClient: &http.Client{Timeout: time.Second}}
	_, readError := client.ServerID(context.Background())
	if readError == nil || !strings.Contains(readError.Error(), "status 404") {
		t.Fatalf("error %v, want a 404 report", readError)
	}
}

func TestDefaultEndpointsTryIPv6First(t *testing.T) {
	if !strings.Contains(DefaultEndpoints[0], "fd00:ec2::254") || !strings.Contains(DefaultEndpoints[1], "169.254.169.254") {
		t.Fatalf("endpoints %v", DefaultEndpoints)
	}
}

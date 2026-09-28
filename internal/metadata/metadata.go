// Package metadata reads the server's own identity from the Ankra Cloud metadata service, which every compute host
// serves to its VMs at fd00:ec2::254 and 169.254.169.254 (agent/crates/ankra-node/src/compute/metadata.rs). The
// EC2-compatible `latest/meta-data/instance-id` answers the server id; the cloud-init `meta-data/instance-id` is a
// different value and is not used.
package metadata

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"
)

// DefaultEndpoints are tried in order: IPv6 first, since Ankra Cloud servers may be IPv6-only.
var DefaultEndpoints = []string{"http://[fd00:ec2::254]", "http://169.254.169.254"}

// ServerIDPath answers the server id as plain text.
const ServerIDPath = "/latest/meta-data/instance-id"

// Client reads the metadata service.
type Client struct {
	Endpoints  []string
	HTTPClient *http.Client
}

// NewClient returns a client for the default endpoints with a short timeout per attempt.
func NewClient() *Client {
	return &Client{Endpoints: DefaultEndpoints, HTTPClient: &http.Client{Timeout: 3 * time.Second}}
}

// ServerID returns the id of the server the caller runs on.
func (client *Client) ServerID(ctx context.Context) (string, error) {
	var failures []string
	for _, endpoint := range client.Endpoints {
		value, readError := client.read(ctx, strings.TrimRight(endpoint, "/")+ServerIDPath)
		if readError == nil && value != "" {
			return value, nil
		}
		if readError == nil {
			readError = errors.New("empty answer")
		}
		failures = append(failures, fmt.Sprintf("%s: %v", endpoint, readError))
	}
	return "", fmt.Errorf("the metadata service did not answer the server id (%s)", strings.Join(failures, "; "))
}

func (client *Client) read(ctx context.Context, target string) (string, error) {
	request, requestError := http.NewRequestWithContext(ctx, http.MethodGet, target, nil)
	if requestError != nil {
		return "", requestError
	}
	response, sendError := client.HTTPClient.Do(request)
	if sendError != nil {
		return "", sendError
	}
	defer func() { _ = response.Body.Close() }()
	body, readError := io.ReadAll(io.LimitReader(response.Body, 4096))
	if readError != nil {
		return "", readError
	}
	if response.StatusCode != http.StatusOK {
		return "", fmt.Errorf("status %d: %s", response.StatusCode, strings.TrimSpace(string(body)))
	}
	return strings.TrimSpace(string(body)), nil
}

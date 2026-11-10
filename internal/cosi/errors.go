package cosi

import (
	"context"
	"errors"
	"fmt"
	"net/http"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	"github.com/ankraio/ankra-cloud-csi/internal/cloud"
)

// apiStatus turns an Ankra Cloud API error into the gRPC status the provisioner sidecar records and retries on.
func apiStatus(callError error, format string, arguments ...any) error {
	if existing, isStatus := status.FromError(callError); isStatus && existing.Code() != codes.Unknown {
		return callError
	}
	message := fmt.Sprintf(format, arguments...)
	if code, isContextError := contextCode(callError); isContextError {
		return status.Errorf(code, "%s: %v", message, callError)
	}
	return status.Errorf(codeForHTTPStatus(cloud.StatusCode(callError)), "%s: %v", message, callError)
}

// s3Status turns an S3 error into a gRPC status.
func s3Status(callError error, format string, arguments ...any) error {
	message := fmt.Sprintf(format, arguments...)
	if code, isContextError := contextCode(callError); isContextError {
		return status.Errorf(code, "%s: %v", message, callError)
	}
	switch {
	case errors.Is(callError, ErrBucketNotFound):
		return status.Errorf(codes.NotFound, "%s: %v", message, callError)
	case errors.Is(callError, ErrAccessDenied):
		return status.Errorf(codes.PermissionDenied, "%s: %v", message, callError)
	case errors.Is(callError, ErrBucketNotEmpty), errors.Is(callError, ErrBucketAlreadyExists):
		return status.Errorf(codes.FailedPrecondition, "%s: %v", message, callError)
	}
	return status.Errorf(codes.Unavailable, "%s: %v", message, callError)
}

func contextCode(callError error) (codes.Code, bool) {
	switch {
	case errors.Is(callError, context.DeadlineExceeded):
		return codes.DeadlineExceeded, true
	case errors.Is(callError, context.Canceled):
		return codes.Canceled, true
	}
	return codes.OK, false
}

func codeForHTTPStatus(statusCode int) codes.Code {
	switch statusCode {
	case http.StatusBadRequest:
		return codes.InvalidArgument
	case http.StatusUnauthorized:
		return codes.Unauthenticated
	case http.StatusPaymentRequired, http.StatusConflict:
		return codes.FailedPrecondition
	case http.StatusForbidden:
		return codes.PermissionDenied
	case http.StatusNotFound:
		return codes.NotFound
	case http.StatusUnprocessableEntity:
		return codes.ResourceExhausted
	case http.StatusTooManyRequests, http.StatusBadGateway, http.StatusServiceUnavailable, http.StatusGatewayTimeout:
		return codes.Unavailable
	}
	return codes.Internal
}

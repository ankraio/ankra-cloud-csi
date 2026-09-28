package driver

import (
	"context"
	"errors"
	"fmt"
	"net/http"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	"github.com/ankraio/ankra-cloud-csi/internal/cloud"
)

// apiStatus turns an API or operation error into the gRPC status the CSI sidecars act on.
func apiStatus(callError error, format string, arguments ...any) error {
	message := fmt.Sprintf(format, arguments...)
	if existing, isStatus := status.FromError(callError); isStatus && existing.Code() != codes.Unknown {
		return callError
	}
	switch {
	case errors.Is(callError, context.DeadlineExceeded):
		return status.Errorf(codes.DeadlineExceeded, "%s: %v", message, callError)
	case errors.Is(callError, context.Canceled):
		return status.Errorf(codes.Canceled, "%s: %v", message, callError)
	case errors.Is(callError, cloud.ErrOperationFailed):
		return status.Errorf(codes.Internal, "%s: %v", message, callError)
	}
	return status.Errorf(codeForHTTPStatus(cloud.StatusCode(callError)), "%s: %v", message, callError)
}

func codeForHTTPStatus(statusCode int) codes.Code {
	switch statusCode {
	case http.StatusBadRequest:
		return codes.InvalidArgument
	case http.StatusUnauthorized:
		return codes.Unauthenticated
	case http.StatusForbidden:
		return codes.PermissionDenied
	case http.StatusNotFound:
		return codes.NotFound
	case http.StatusConflict:
		return codes.FailedPrecondition
	case http.StatusUnprocessableEntity:
		return codes.ResourceExhausted
	case http.StatusTooManyRequests, http.StatusBadGateway, http.StatusServiceUnavailable, http.StatusGatewayTimeout:
		return codes.Unavailable
	}
	return codes.Internal
}

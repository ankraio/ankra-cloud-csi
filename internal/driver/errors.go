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

// errVolumeRemains is returned when the API still has a storage after the driver asked for it to be deleted and the
// delete is no longer running: the API puts a storage whose delete failed back to `error`.
var errVolumeRemains = errors.New("the storage is still there")

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

// pendingStatus is the status CreateVolume answers with while a storage of the volume's name exists, or may exist, and
// the call can neither hand it over nor confirm that it is gone.
//
// external-provisioner reads the code of a failed CreateVolume to decide whether provisioning is still going on
// (checkError in external-provisioner, ProvisioningInBackground in sig-storage-lib-external-provisioner). After
// Canceled, DeadlineExceeded, Unavailable or Aborted it is: the provisioner keeps calling CreateVolume with the same
// name, also once the claim is deleted, until a call succeeds or answers another code, and a volume that arrives for a
// claim that is gone is deleted again through DeleteVolume. After any other code nothing is in progress: the
// provisioner calls again only while the claim exists, so a storage left under the name of a claim that is gone is
// never looked at again. No PersistentVolume was made for it, so DeleteVolume is never called either.
// ResourceExhausted is such a code too once a node was selected: the claim goes back to the scheduler.
//
// A code that ends provisioning is therefore safe only once no storage of the name is left. Until then the answer is
// DeadlineExceeded when the call's deadline passed, Canceled when the caller gave up, Unavailable when a call to the
// API failed (cause is its error), and Aborted when the API answered and work on the storage is still to be done (no
// cause, a failed operation, or a storage that is still there after its delete).
func pendingStatus(ctx context.Context, cause error, format string, arguments ...any) error {
	message := fmt.Sprintf(format, arguments...)
	if cause != nil {
		message = fmt.Sprintf("%s: %v", message, cause)
	}
	switch {
	case errors.Is(ctx.Err(), context.DeadlineExceeded), errors.Is(cause, context.DeadlineExceeded):
		return status.Error(codes.DeadlineExceeded, message)
	case errors.Is(ctx.Err(), context.Canceled), errors.Is(cause, context.Canceled):
		return status.Error(codes.Canceled, message)
	case cause == nil, errors.Is(cause, cloud.ErrOperationFailed), errors.Is(cause, errVolumeRemains):
		return status.Error(codes.Aborted, message)
	}
	return status.Error(codes.Unavailable, message)
}

// codeForHTTPStatus is the gRPC code of an API answer. An error without an HTTP status (the call got no answer, or one
// that could not be read) and a 500 are Unavailable, not Internal: the API may have carried the request out all the
// same, and the sidecars take Internal to mean that nothing is in progress.
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
	case 0, http.StatusInternalServerError, http.StatusTooManyRequests, http.StatusBadGateway, http.StatusServiceUnavailable,
		http.StatusGatewayTimeout:
		return codes.Unavailable
	}
	return codes.Internal
}

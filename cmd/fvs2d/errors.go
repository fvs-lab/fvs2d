package main

import (
	"context"
	"errors"

	fvsrepo "fvs2/repo"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

// mapError classifies an error from the fvs2 repo package (or the daemon's
// own path guard/context handling) into the gRPC code that best describes
// it, instead of the blanket codes.FailedPrecondition every handler used to
// return. Unrecognized errors still fall back to FailedPrecondition, so
// behavior for anything not listed below is unchanged.
func mapError(err error) error {
	if err == nil {
		return nil
	}
	if _, ok := status.FromError(err); ok {
		// Already a gRPC status (e.g. an InvalidArgument built directly by a
		// handler); pass it through untouched.
		return err
	}
	switch {
	case errors.Is(err, errPathEscape):
		return status.Error(codes.PermissionDenied, err.Error())
	case errors.Is(err, context.Canceled):
		return status.Error(codes.Canceled, err.Error())
	case errors.Is(err, context.DeadlineExceeded):
		return status.Error(codes.DeadlineExceeded, err.Error())
	case errors.Is(err, fvsrepo.ErrStateNotFound):
		return status.Error(codes.NotFound, err.Error())
	case errors.Is(err, fvsrepo.ErrLockTimeout):
		return status.Error(codes.Aborted, err.Error())
	case errors.Is(err, fvsrepo.ErrFormatUnsupported):
		return status.Error(codes.FailedPrecondition, err.Error())
	default:
		return status.Error(codes.FailedPrecondition, err.Error())
	}
}

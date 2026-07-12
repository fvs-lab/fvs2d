package main

import (
	"context"
	"crypto/subtle"
	"strings"

	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/metadata"
	"google.golang.org/grpc/status"
)

// tokenMetadataKey is the preferred gRPC metadata header carrying the shared
// control-API token. "authorization: Bearer <token>" is also accepted.
const tokenMetadataKey = "x-fvs2d-token"

// tokenUnaryInterceptor rejects any unary call whose metadata does not carry
// a token matching want, compared in constant time.
func tokenUnaryInterceptor(want string) grpc.UnaryServerInterceptor {
	return func(ctx context.Context, req any, info *grpc.UnaryServerInfo, handler grpc.UnaryHandler) (any, error) {
		if err := checkToken(ctx, want); err != nil {
			return nil, err
		}
		return handler(ctx, req)
	}
}

// tokenStreamInterceptor is tokenUnaryInterceptor for streaming RPCs.
func tokenStreamInterceptor(want string) grpc.StreamServerInterceptor {
	return func(srv any, ss grpc.ServerStream, info *grpc.StreamServerInfo, handler grpc.StreamHandler) error {
		if err := checkToken(ss.Context(), want); err != nil {
			return err
		}
		return handler(srv, ss)
	}
}

func checkToken(ctx context.Context, want string) error {
	md, ok := metadata.FromIncomingContext(ctx)
	if !ok {
		return status.Error(codes.Unauthenticated, "missing control token")
	}
	got := firstMetadata(md, tokenMetadataKey)
	if got == "" {
		got = strings.TrimPrefix(firstMetadata(md, "authorization"), "Bearer ")
	}
	if got == "" || !constantTimeEqual(got, want) {
		return status.Error(codes.Unauthenticated, "invalid control token")
	}
	return nil
}

func firstMetadata(md metadata.MD, key string) string {
	v := md.Get(key)
	if len(v) == 0 {
		return ""
	}
	return v[0]
}

// constantTimeEqual compares two tokens without leaking their contents
// through timing. Differing lengths are rejected outright (subtle.
// ConstantTimeCompare requires equal-length inputs); the length check itself
// leaks only the (public) expected token length, not its value.
func constantTimeEqual(a, b string) bool {
	if len(a) != len(b) {
		return false
	}
	return subtle.ConstantTimeCompare([]byte(a), []byte(b)) == 1
}

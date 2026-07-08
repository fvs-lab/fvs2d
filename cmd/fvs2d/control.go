package main

import (
	"context"
	"fmt"
	"net"
	"os"
	"strings"

	"google.golang.org/grpc"
	"google.golang.org/grpc/health"
	healthpb "google.golang.org/grpc/health/grpc_health_v1"
	"google.golang.org/protobuf/types/known/emptypb"

	pb "fvs2d/internal/controlpb"
)

const controlAPIVersion = "1"

// controlServer implements the gRPC Control service. Daemon behaviour is
// injected as closures so this file holds no FUSE/cgo state.
type controlServer struct {
	pb.UnimplementedControlServer
	statusFn   func() *pb.GetStatusResponse
	shutdownFn func(lazy bool)
}

func (s *controlServer) GetStatus(_ context.Context, _ *pb.GetStatusRequest) (*pb.GetStatusResponse, error) {
	return s.statusFn(), nil
}

func (s *controlServer) Shutdown(_ context.Context, req *pb.ShutdownRequest) (*emptypb.Empty, error) {
	s.shutdownFn(req.GetLazy())
	return &emptypb.Empty{}, nil
}

// parseControlAddr maps a -control value onto net.Listen (network, address).
//
// Accepted forms:
//
//	unix:/path/to.sock   unix domain socket
//	/path/to.sock        unix domain socket (leading slash)
//	tcp:host:port        TCP
//	host:port            TCP
func parseControlAddr(addr string) (network, address string, err error) {
	switch {
	case addr == "":
		return "", "", fmt.Errorf("empty control address")
	case strings.HasPrefix(addr, "unix:"):
		return "unix", strings.TrimPrefix(addr, "unix:"), nil
	case strings.HasPrefix(addr, "tcp:"):
		return "tcp", strings.TrimPrefix(addr, "tcp:"), nil
	case strings.HasPrefix(addr, "/"):
		return "unix", addr, nil
	default:
		return "tcp", addr, nil
	}
}

// startControlServer serves the control API on addr and returns the running
// server so the caller can stop it during shutdown.
func startControlServer(addr string, srv *controlServer) (*grpc.Server, error) {
	network, address, err := parseControlAddr(addr)
	if err != nil {
		return nil, err
	}
	if network == "unix" {
		// Clear a stale socket from a previous run so Listen does not fail.
		if err := os.Remove(address); err != nil && !os.IsNotExist(err) {
			return nil, fmt.Errorf("control: remove stale socket: %w", err)
		}
	}
	lis, err := net.Listen(network, address)
	if err != nil {
		return nil, fmt.Errorf("control: listen %s %s: %w", network, address, err)
	}
	if network == "unix" {
		// Local control only: restrict the socket to the owner.
		if err := os.Chmod(address, 0o600); err != nil {
			_ = lis.Close()
			return nil, fmt.Errorf("control: chmod socket: %w", err)
		}
	}
	gs := grpc.NewServer()
	pb.RegisterControlServer(gs, srv)
	hs := health.NewServer()
	hs.SetServingStatus("", healthpb.HealthCheckResponse_SERVING)
	healthpb.RegisterHealthServer(gs, hs)
	go func() {
		// Serve returns when the server is stopped; the error is not actionable.
		_ = gs.Serve(lis)
	}()
	return gs, nil
}

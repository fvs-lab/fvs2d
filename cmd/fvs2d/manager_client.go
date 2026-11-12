package main

import (
	"context"
	"fmt"
	"net"
	"time"

	"github.com/fvs-lab/fvs2d/fvs2dpb"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/credentials/insecure"
	"google.golang.org/grpc/metadata"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/types/known/emptypb"
)

func probeManager(address, token string) (*fvs2dpb.ProbeResponse, error) {
	network, endpoint, err := parseControlAddr(address)
	if err != nil {
		return nil, err
	}
	connection, err := grpc.NewClient("passthrough:///fvs2d", grpc.WithTransportCredentials(insecure.NewCredentials()), grpc.WithContextDialer(func(ctx context.Context, _ string) (net.Conn, error) {
		return (&net.Dialer{}).DialContext(ctx, network, endpoint)
	}))
	if err != nil {
		return nil, err
	}
	defer connection.Close()
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	if token != "" {
		ctx = metadata.AppendToOutgoingContext(ctx, "x-fvs2d-token", token)
	}
	response, err := fvs2dpb.NewFvs2DClient(connection).Probe(ctx, &emptypb.Empty{})
	if err != nil {
		if status.Code(err) == codes.Unauthenticated || status.Code(err) == codes.PermissionDenied {
			return nil, fmt.Errorf("mount manager rejected authentication: %w", err)
		}
		return nil, fmt.Errorf("mount manager unavailable at %s: %w", address, err)
	}
	return response, nil
}

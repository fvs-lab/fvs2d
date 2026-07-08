package main

import (
	"context"
	"net"
	"path/filepath"
	"testing"
	"time"

	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials/insecure"

	pb "fvs2d/internal/controlpb"
)

func dialControl(t *testing.T, addr string) (pb.ControlClient, *grpc.ClientConn) {
	t.Helper()
	network, address, err := parseControlAddr(addr)
	if err != nil {
		t.Fatalf("parseControlAddr(%q): %v", addr, err)
	}
	conn, err := grpc.NewClient(
		"passthrough:///"+address,
		grpc.WithTransportCredentials(insecure.NewCredentials()),
		grpc.WithContextDialer(func(ctx context.Context, _ string) (net.Conn, error) {
			return (&net.Dialer{}).DialContext(ctx, network, address)
		}),
	)
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	return pb.NewControlClient(conn), conn
}

func TestControlServerRoundTrip(t *testing.T) {
	sock := filepath.Join(t.TempDir(), "control.sock")
	addr := "unix:" + sock

	shutdownCh := make(chan bool, 1)
	srv := &controlServer{
		statusFn: func() *pb.GetStatusResponse {
			return &pb.GetStatusResponse{
				Mountpoint: "/mnt/x",
				Writable:   true,
				Upper:      "/work/upper",
				NodeCount:  7,
				BlockSize:  4096,
				ApiVersion: controlAPIVersion,
			}
		},
		shutdownFn: func(lazy bool) { shutdownCh <- lazy },
	}

	gs, err := startControlServer(addr, srv)
	if err != nil {
		t.Fatalf("startControlServer: %v", err)
	}
	defer gs.Stop()

	client, conn := dialControl(t, addr)
	defer conn.Close()

	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()

	h, err := client.Health(ctx, &pb.HealthRequest{})
	if err != nil || !h.GetOk() {
		t.Fatalf("Health: err=%v ok=%v", err, h.GetOk())
	}

	st, err := client.GetStatus(ctx, &pb.GetStatusRequest{})
	if err != nil {
		t.Fatalf("GetStatus: %v", err)
	}
	if st.GetMountpoint() != "/mnt/x" || !st.GetWritable() || st.GetNodeCount() != 7 ||
		st.GetUpper() != "/work/upper" || st.GetApiVersion() != controlAPIVersion {
		t.Fatalf("unexpected status: %+v", st)
	}

	resp, err := client.Shutdown(ctx, &pb.ShutdownRequest{Lazy: true})
	if err != nil || !resp.GetOk() {
		t.Fatalf("Shutdown: err=%v ok=%v", err, resp.GetOk())
	}
	select {
	case lazy := <-shutdownCh:
		if !lazy {
			t.Fatal("expected lazy=true to reach the shutdown closure")
		}
	case <-time.After(2 * time.Second):
		t.Fatal("shutdown closure was not invoked")
	}
}

func TestParseControlAddr(t *testing.T) {
	cases := []struct {
		in      string
		network string
		address string
		wantErr bool
	}{
		{"unix:/run/fvs2d.sock", "unix", "/run/fvs2d.sock", false},
		{"/run/fvs2d.sock", "unix", "/run/fvs2d.sock", false},
		{"tcp:127.0.0.1:50071", "tcp", "127.0.0.1:50071", false},
		{"127.0.0.1:50071", "tcp", "127.0.0.1:50071", false},
		{"", "", "", true},
	}
	for _, c := range cases {
		n, a, err := parseControlAddr(c.in)
		if c.wantErr {
			if err == nil {
				t.Errorf("parseControlAddr(%q): expected error", c.in)
			}
			continue
		}
		if err != nil || n != c.network || a != c.address {
			t.Errorf("parseControlAddr(%q) = (%q, %q, %v), want (%q, %q, nil)",
				c.in, n, a, err, c.network, c.address)
		}
	}
}

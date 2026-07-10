package main

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	"fvs2d/internal/fvs2dpb"
)

func TestParseControlAddr(t *testing.T) {
	tests := []struct {
		in, network, address string
		wantErr              bool
	}{
		{"unix:/run/fvs2d.sock", "unix", "/run/fvs2d.sock", false},
		{"/run/fvs2d.sock", "unix", "/run/fvs2d.sock", false},
		{"tcp:127.0.0.1:50071", "tcp", "127.0.0.1:50071", false},
		{"127.0.0.1:50071", "tcp", "127.0.0.1:50071", false},
		{"", "", "", true},
	}
	for _, tt := range tests {
		network, address, err := parseControlAddr(tt.in)
		if tt.wantErr {
			if err == nil {
				t.Errorf("parseControlAddr(%q): expected error", tt.in)
			}
			continue
		}
		if err != nil || network != tt.network || address != tt.address {
			t.Errorf("parseControlAddr(%q) = (%q, %q, %v), want (%q, %q, nil)", tt.in, network, address, err, tt.network, tt.address)
		}
	}
}

func TestRepositoryRPCs(t *testing.T) {
	root := t.TempDir()
	service := &fvs2dService{mgr: newMountManager(), shutdown: func(bool) {}}

	repository, err := service.InitRepository(context.Background(), &fvs2dpb.InitRepositoryRequest{
		RepositoryPath: root,
		BlockSize:      8192,
	})
	if err != nil {
		t.Fatal(err)
	}
	if repository.GetRepositoryPath() != root || repository.GetBlockSize() != 8192 {
		t.Fatalf("repository = %+v", repository)
	}
	if err := os.WriteFile(filepath.Join(root, "file"), []byte("content"), 0o644); err != nil {
		t.Fatal(err)
	}
	revision, err := service.Commit(context.Background(), &fvs2dpb.CommitRequest{RepositoryPath: root, Message: "first"})
	if err != nil {
		t.Fatal(err)
	}
	if revision.GetStateId() == "" || revision.GetFileCount() != 1 || !revision.GetCreatedAt().IsValid() {
		t.Fatalf("revision = %+v", revision)
	}
}

func TestStatesAndRestoreRPCs(t *testing.T) {
	root := t.TempDir()
	service := &fvs2dService{mgr: newMountManager(), shutdown: func(bool) {}}
	ctx := context.Background()

	if _, err := service.InitRepository(ctx, &fvs2dpb.InitRepositoryRequest{RepositoryPath: root}); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, "file"), []byte("v1"), 0o644); err != nil {
		t.Fatal(err)
	}
	first, err := service.Commit(ctx, &fvs2dpb.CommitRequest{RepositoryPath: root, Message: "first"})
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, "file"), []byte("v2 changed"), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := service.Commit(ctx, &fvs2dpb.CommitRequest{RepositoryPath: root, Message: "second"}); err != nil {
		t.Fatal(err)
	}

	states, err := service.ListStates(ctx, &fvs2dpb.ListStatesRequest{RepositoryPath: root})
	if err != nil {
		t.Fatal(err)
	}
	if len(states.GetStates()) != 2 {
		t.Fatalf("states = %d, want 2", len(states.GetStates()))
	}

	restored, err := service.Restore(ctx, &fvs2dpb.RestoreRequest{
		RepositoryPath:  root,
		StateIdOrPrefix: first.GetStateId(),
		Clean:           true,
		Reset_:          true,
	})
	if err != nil {
		t.Fatal(err)
	}
	if restored.GetStateId() != first.GetStateId() {
		t.Fatalf("restored %s, want %s", restored.GetStateId(), first.GetStateId())
	}
	content, err := os.ReadFile(filepath.Join(root, "file"))
	if err != nil {
		t.Fatal(err)
	}
	if string(content) != "v1" {
		t.Fatalf("file = %q after rollback, want %q", content, "v1")
	}
}

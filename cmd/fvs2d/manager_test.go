package main

import "testing"

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

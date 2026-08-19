// Copyright (c) HashiCorp, Inc.
// SPDX-License-Identifier: MPL-2.0

package docker

import (
	"os"
	"path/filepath"
	"slices"
	"testing"

	docker "github.com/moby/moby/client"
)

// TestDockerClusterNode_apiHost covers the host-selection logic used to reach
// ports published by a node's container. This matters most for CI runners
// that use a dind (Docker-in-Docker) daemon reached over TCP, where the
// daemon - and therefore the published port - lives in a different container
// than the test process, so 127.0.0.1 is not reachable.
func TestDockerClusterNode_apiHost(t *testing.T) {
	tests := []struct {
		name        string
		envOverride string
		daemonHost  string // if set, backs n.dockerAPI with a client using this DOCKER_HOST-style value
		want        string
	}{
		{
			name: "no docker client configured defaults to loopback",
			want: "127.0.0.1",
		},
		{
			name:       "unix socket daemon uses loopback",
			daemonHost: "unix:///var/run/docker.sock",
			want:       "127.0.0.1",
		},
		{
			name:       "tcp daemon uses daemon hostname",
			daemonHost: "tcp://docker:2375",
			want:       "docker",
		},
		{
			name:       "https daemon uses daemon hostname",
			daemonHost: "https://docker:2376",
			want:       "docker",
		},
		{
			name:        "env override wins over tcp daemon",
			envOverride: "override-host",
			daemonHost:  "tcp://docker:2375",
			want:        "override-host",
		},
		{
			name:        "env override wins with no docker client configured",
			envOverride: "override-host",
			want:        "override-host",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if tt.envOverride != "" {
				t.Setenv(DockerHostEnvVar, tt.envOverride)
			}

			n := &dockerClusterNode{}
			if tt.daemonHost != "" {
				cli, err := docker.New(docker.WithHost(tt.daemonHost))
				if err != nil {
					t.Fatalf("failed to create docker client for host %q: %v", tt.daemonHost, err)
				}
				t.Cleanup(func() { cli.Close() })
				n.dockerAPI = cli
			}

			if got := n.apiHost(); got != tt.want {
				t.Errorf("apiHost() = %q, want %q", got, tt.want)
			}
		})
	}
}

// TestDockerClusterNode_setupCert_includesAPIHost guards against the cert
// only covering localhost/the node's own container name: the test process
// connects using apiHost() (e.g. the dind daemon's hostname), so the server
// cert must also be valid for that host or TLS verification fails.
func TestDockerClusterNode_setupCert_includesAPIHost(t *testing.T) {
	tmpDir := t.TempDir()
	if err := os.MkdirAll(filepath.Join(tmpDir, "ca"), 0o755); err != nil {
		t.Fatalf("failed to create ca dir: %v", err)
	}

	dc := &Cluster{tmpDir: tmpDir}
	if err := dc.setupCA(nil); err != nil {
		t.Fatalf("setupCA() error = %v", err)
	}

	cli, err := docker.New(docker.WithHost("tcp://docker:2375"))
	if err != nil {
		t.Fatalf("failed to create docker client: %v", err)
	}
	t.Cleanup(func() { cli.Close() })

	n := &dockerClusterNode{
		NodeID:    "vault-0",
		Cluster:   dc,
		WorkDir:   t.TempDir(),
		dockerAPI: cli,
	}

	if err := n.setupCert(); err != nil {
		t.Fatalf("setupCert() error = %v", err)
	}

	if !slices.Contains(n.ServerCert.DNSNames, "docker") {
		t.Errorf("ServerCert.DNSNames = %v, want it to contain %q", n.ServerCert.DNSNames, "docker")
	}
}

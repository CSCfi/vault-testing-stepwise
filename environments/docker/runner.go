// Copyright (c) HashiCorp, Inc.
// SPDX-License-Identifier: MPL-2.0

package docker

import (
	"archive/tar"
	"context"
	"fmt"
	"io"
	"net/netip"

	"github.com/moby/go-archive"
	"github.com/moby/moby/api/types/container"
	"github.com/moby/moby/api/types/network"
	docker "github.com/moby/moby/client"
)

// Runner manages the lifecycle of the Docker container
type Runner struct {
	dockerAPI       *docker.Client
	ContainerConfig *container.Config
	ContainerName   string
	NetName         string
	IP              string
	CopyFromTo      map[string]string
}

// Start is responsible for executing the Vault container. It consists of
// pulling the specified Vault image, creating the container, and copies the
// plugin binary into the container file system before starting the container
// itself.
func (d *Runner) Start(ctx context.Context) (*container.InspectResponse, error) {
	hostConfig := &container.HostConfig{
		PublishAllPorts: true,
		AutoRemove:      true,
		CapAdd:          []string{"IPC_LOCK", "NET_ADMIN"},
		PortBindings:    network.PortMap{},
	}

	networkingConfig := &network.NetworkingConfig{}
	switch d.NetName {
	case "":
	case "host":
		hostConfig.NetworkMode = "host"
	default:
		es := &network.EndpointSettings{
			Aliases: []string{d.ContainerName},
		}
		if len(d.IP) != 0 {
			runnerIP, err := netip.ParseAddr(d.IP)
			if err != nil {
				return nil, fmt.Errorf("runner has invalid IP %s: %v", d.IP, err)
			}

			es.IPAMConfig = &network.EndpointIPAMConfig{
				IPv4Address: runnerIP,
			}
		}
		networkingConfig.EndpointsConfig = map[string]*network.EndpointSettings{
			d.NetName: es,
		}
	}

	// Best-effort pull. ImageCreate here will use a matching image from the local
	// Docker library, or if not found pull the matching image from docker hub. If
	// not found on docker hub, returns an error. The response must be read in
	// order for the local image to be used.
	resp, err := d.dockerAPI.ImagePull(ctx, d.ContainerConfig.Image, docker.ImagePullOptions{})
	if err != nil {
		return nil, err
	}
	if resp != nil {
		_, _ = io.ReadAll(resp)
	}

	cfg := *d.ContainerConfig
	cfg.Hostname = d.ContainerName
	fullName := d.ContainerName
	newContainer, err := d.dockerAPI.ContainerCreate(ctx, docker.ContainerCreateOptions{
		Config: &cfg, HostConfig: hostConfig, NetworkingConfig: networkingConfig, Name: fullName,
	})
	if err != nil {
		return nil, fmt.Errorf("container create failed: %v", err)
	}

	// copies the plugin binary into the Docker file system. This copy is only
	// allowed before the container is started
	for from, to := range d.CopyFromTo {
		if err := copyToContainer(ctx, d.dockerAPI, newContainer.ID, from, to); err != nil {
			_, _ = d.dockerAPI.ContainerRemove(ctx, newContainer.ID, docker.ContainerRemoveOptions{})

			return nil, err
		}
	}

	_, err = d.dockerAPI.ContainerStart(ctx, newContainer.ID, docker.ContainerStartOptions{})
	if err != nil {
		_, _ = d.dockerAPI.ContainerRemove(ctx, newContainer.ID, docker.ContainerRemoveOptions{})

		return nil, fmt.Errorf("container start failed: %v", err)
	}

	inspect, err := d.dockerAPI.ContainerInspect(ctx, newContainer.ID, docker.ContainerInspectOptions{})
	if err != nil {
		_, _ = d.dockerAPI.ContainerRemove(ctx, newContainer.ID, docker.ContainerRemoveOptions{})

		return nil, err
	}

	return &inspect.Container, nil
}

func copyToContainer(ctx context.Context, d *docker.Client, containerID, from, to string) error {
	srcInfo, err := archive.CopyInfoSourcePath(from, false)
	if err != nil {
		return fmt.Errorf("error copying from source %q: %v", from, err)
	}

	srcArchive, err := archive.TarResource(srcInfo)
	if err != nil {
		return fmt.Errorf("error creating tar from source %q: %v", from, err)
	}
	defer srcArchive.Close()

	dstInfo := archive.CopyInfo{Path: to}

	dstDir, content, err := archive.PrepareArchiveCopy(srcArchive, srcInfo, dstInfo)
	if err != nil {
		return fmt.Errorf("error preparing copy from %q -> %q: %v", from, to, err)
	}
	defer content.Close()

	normalizedContent := normalizePerms(content)
	defer normalizedContent.Close()

	_, err = d.CopyToContainer(ctx, containerID, docker.CopyToContainerOptions{Content: normalizedContent, DestinationPath: dstDir})
	if err != nil {
		return fmt.Errorf("error copying from %q -> %q: %v", from, to, err)
	}

	return nil
}

// normalizePerms rewrites tar headers so that files are 0644 and directories
// are 0755 inside the container, regardless of the host filesystem permissions.
// moby preserves host modes verbatim, which breaks containers that
// run as a non-root user when the source was created with restrictive modes
// such as 0600 or 0700.
func normalizePerms(r io.ReadCloser) io.ReadCloser {
	pr, pw := io.Pipe()
	go func() {
		tr := tar.NewReader(r)
		tw := tar.NewWriter(pw)
		for {
			hdr, err := tr.Next()
			if err == io.EOF {
				_ = tw.Close()
				pw.Close()

				return
			}
			if err != nil {
				pw.CloseWithError(err)

				return
			}
			if hdr.Typeflag == tar.TypeDir || hdr.Mode&0o111 != 0 {
				hdr.Mode = 0o755
			} else {
				hdr.Mode = 0o644
			}
			if err := tw.WriteHeader(hdr); err != nil {
				pw.CloseWithError(err)

				return
			}
			if _, err := io.Copy(tw, tr); err != nil {
				pw.CloseWithError(err)

				return
			}
		}
	}()

	return pr
}

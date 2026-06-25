// Copyright (c) HashiCorp, Inc.
// SPDX-License-Identifier: MPL-2.0

package docker

import (
	"archive/tar"
	"context"
	"fmt"
	"io"
	"net/netip"
	"strings"

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

	if err := d.ensureImage(ctx, d.ContainerConfig.Image); err != nil {
		return nil, err
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

// ensureImage pulls the image if needed. For mutable tags (latest or no tag)
// it always pulls so the local cache stays current. For immutable version tags
// it skips the pull when the image is already present locally.
func (d *Runner) ensureImage(ctx context.Context, image string) error {
	if !isLatestTag(image) {
		if _, err := d.dockerAPI.ImageInspect(ctx, image); err == nil {
			return nil
		}
	}

	resp, err := d.dockerAPI.ImagePull(ctx, image, docker.ImagePullOptions{})
	if err != nil {
		return fmt.Errorf("failed to pull image %q: %w", image, err)
	}
	if resp != nil {
		_, _ = io.ReadAll(resp)
		resp.Close()
	}

	return nil
}

// isLatestTag reports whether an image reference uses a mutable tag (no tag or
// "latest"). Registry:port prefixes such as "registry:5000/image" are handled
// correctly: the colon in "registry:5000" is followed by a slash, so it is not
// mistaken for a tag separator.
func isLatestTag(image string) bool {
	i := strings.LastIndex(image, ":")
	if i == -1 {
		return true
	}

	tag := image[i+1:]
	if strings.ContainsRune(tag, '/') {
		return true // colon was part of registry:port, not a tag separator
	}

	return tag == "" || tag == "latest"
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

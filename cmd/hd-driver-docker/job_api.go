// Copyright 2026 Chun Huang (Charles).

// This Source Code Form is dual-licensed.
// By default, this file is licensed under the GNU Affero General Public License v3.0.
// If you have a separate written commercial agreement, you may use this file under those terms instead.

package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"strconv"
	"strings"

	"github.com/containerd/errdefs"
	"github.com/docker/docker/api/types/container"
	"github.com/docker/docker/api/types/filters"
	"github.com/docker/docker/api/types/image"
	"github.com/docker/docker/api/types/network"
	"github.com/docker/docker/api/types/volume"
	"github.com/docker/docker/client"
)

// jobContainer describes one container of a job as discovered by its job label.
// The Role and Index fields are recorded at create time by resolveRoles, so the
// "last container is main" rule is never re-encoded at read time.
type jobContainer struct {
	ID              string
	Name            string
	Role            string // "init" or "main"
	Index           int
	WorkVolume      string
	WorkVolumeOwned bool
}

// jobAPI abstracts the Docker Engine operations the job lifecycle needs, so the
// sequencing and cleanup logic is unit-testable against a fake implementation.
type jobAPI interface {
	ImageExists(ctx context.Context, ref string) (bool, error)
	PullImage(ctx context.Context, ref string) error
	CreateContainer(ctx context.Context, name string, cfg *container.Config, host *container.HostConfig, net *network.NetworkingConfig) (string, error)
	StartContainer(ctx context.Context, id string) error
	WaitContainer(ctx context.Context, id string) (int64, error)
	StopContainer(ctx context.Context, id string) error
	InspectContainer(ctx context.Context, id string) (containerState, error)
	RemoveContainer(ctx context.Context, id string, force bool) error
	ListContainers(ctx context.Context, jobID string) ([]jobContainer, error)
	ContainerLogs(ctx context.Context, id string, opts container.LogsOptions) (io.ReadCloser, error)
	VolumeExists(ctx context.Context, name string) (bool, error)
	CreateVolume(ctx context.Context, name string) error
	RemoveVolume(ctx context.Context, name string) error
}

// dockerJobAPI is the production jobAPI backed by the Docker Engine API client.
type dockerJobAPI struct {
	cli *client.Client
}

// newDockerJobAPI wraps a Docker Engine API client in the jobAPI abstraction.
func newDockerJobAPI(cli *client.Client) jobAPI {
	return &dockerJobAPI{cli: cli}
}

func (d *dockerJobAPI) ImageExists(ctx context.Context, ref string) (bool, error) {
	_, err := d.cli.ImageInspect(ctx, ref)
	if err == nil {
		return true, nil
	}
	if errdefs.IsNotFound(err) {
		return false, nil
	}

	return false, err
}

func (d *dockerJobAPI) PullImage(ctx context.Context, ref string) error {
	rc, err := d.cli.ImagePull(ctx, ref, image.PullOptions{})
	if err != nil {
		return err
	}
	defer func() { _ = rc.Close() }()
	// ImagePull returns a stream of JSON progress messages; failures (auth,
	// manifest unknown, network) arrive as {"error": ...} messages IN the stream
	// rather than as an error from ImagePull, so the stream must be scanned for
	// them instead of being discarded.
	return pullStreamError(rc, ref)
}

// pullErrorMessage is the subset of an ImagePull progress message that carries a
// failure. The daemon emits {"errorDetail": {"message": ...}} (and, in older
// versions, a top-level "error" field) when a pull fails.
type pullErrorMessage struct {
	ErrorDetail *struct{ Message string } `json:"errorDetail"`
	Error       string                    `json:"error"`
}

// pullStreamError scans an ImagePull JSON progress stream and returns the first
// error message found (wrapped with the image ref), or nil when the pull
// succeeded. Without this, a failed pull would be reported as done and only
// surface later as a confusing "No such image" error from CreateContainer.
func pullStreamError(rc io.Reader, ref string) error {
	dec := json.NewDecoder(rc)
	for {
		var msg pullErrorMessage
		if err := dec.Decode(&msg); err != nil {
			if errors.Is(err, io.EOF) {
				return nil
			}

			return fmt.Errorf("error reading image pull stream for %s: %w", ref, err)
		}
		if msg.ErrorDetail != nil && msg.ErrorDetail.Message != "" {
			return fmt.Errorf("image pull %s failed: %s", ref, msg.ErrorDetail.Message)
		}
		if msg.Error != "" {
			return fmt.Errorf("image pull %s failed: %s", ref, msg.Error)
		}
	}
}

func (d *dockerJobAPI) CreateContainer(ctx context.Context, name string, cfg *container.Config, host *container.HostConfig, net *network.NetworkingConfig) (string, error) {
	resp, err := d.cli.ContainerCreate(ctx, cfg, host, net, nil, name)
	if err != nil {
		return "", err
	}

	return resp.ID, nil
}

func (d *dockerJobAPI) StartContainer(ctx context.Context, id string) error {
	return d.cli.ContainerStart(ctx, id, container.StartOptions{})
}

func (d *dockerJobAPI) WaitContainer(ctx context.Context, id string) (int64, error) {
	resultC, errC := d.cli.ContainerWait(ctx, id, container.WaitConditionNotRunning)
	select {
	case res := <-resultC:
		return res.StatusCode, nil
	case err := <-errC:
		return 0, err
	case <-ctx.Done():
		return 0, ctx.Err()
	}
}

func (d *dockerJobAPI) StopContainer(ctx context.Context, id string) error {
	return d.cli.ContainerStop(ctx, id, container.StopOptions{})
}

func (d *dockerJobAPI) InspectContainer(ctx context.Context, id string) (containerState, error) {
	inspect, err := d.cli.ContainerInspect(ctx, id)
	if err != nil {
		return containerState{}, err
	}
	state := containerState{
		ID:   inspect.ID,
		Name: strings.TrimPrefix(inspect.Name, "/"),
	}
	if inspect.State != nil {
		state.Status = inspect.State.Status
		state.ExitCode = inspect.State.ExitCode
		state.Running = inspect.State.Running
	}
	// Whether the container was created with a TTY changes the encoding of its
	// Docker log stream (framed vs. raw), so the log decoding needs it.
	state.Tty = inspect.Config != nil && inspect.Config.Tty

	return state, nil
}

func (d *dockerJobAPI) RemoveContainer(ctx context.Context, id string, force bool) error {
	return d.cli.ContainerRemove(ctx, id, container.RemoveOptions{Force: force})
}

func (d *dockerJobAPI) ListContainers(ctx context.Context, jobID string) ([]jobContainer, error) {
	f := filters.NewArgs(filters.Arg("label", "hd-job="+jobID))
	summaries, err := d.cli.ContainerList(ctx, container.ListOptions{All: true, Filters: f})
	if err != nil {
		return nil, err
	}
	ret := make([]jobContainer, 0, len(summaries))
	for _, s := range summaries {
		item := jobContainer{
			ID:              s.ID,
			Name:            firstContainerName(s.Names),
			Role:            s.Labels["hd-role"],
			WorkVolume:      s.Labels["hd-work-volume"],
			WorkVolumeOwned: s.Labels["hd-work-volume-owned"] == "true",
		}
		if idx, ok := parseIndex(s.Labels["hd-index"]); ok {
			item.Index = idx
		}
		ret = append(ret, item)
	}

	return ret, nil
}

func (d *dockerJobAPI) ContainerLogs(ctx context.Context, id string, opts container.LogsOptions) (io.ReadCloser, error) {
	return d.cli.ContainerLogs(ctx, id, opts)
}

func (d *dockerJobAPI) VolumeExists(ctx context.Context, name string) (bool, error) {
	_, err := d.cli.VolumeInspect(ctx, name)
	if err == nil {
		return true, nil
	}
	if errdefs.IsNotFound(err) {
		return false, nil
	}

	return false, err
}

func (d *dockerJobAPI) CreateVolume(ctx context.Context, name string) error {
	_, err := d.cli.VolumeCreate(ctx, volume.CreateOptions{Name: name})

	return err
}

func (d *dockerJobAPI) RemoveVolume(ctx context.Context, name string) error {
	return d.cli.VolumeRemove(ctx, name, false)
}

// firstContainerName returns the first container name, stripping the leading
// slash Docker prepends to names.
func firstContainerName(names []string) string {
	for _, n := range names {
		if n = strings.TrimPrefix(n, "/"); n != "" {
			return n
		}
	}

	return ""
}

// parseIndex parses an `hd-index` label value.
func parseIndex(v string) (int, bool) {
	i, err := strconv.Atoi(strings.TrimSpace(v))
	if err != nil {
		return 0, false
	}

	return i, true
}

// isNotFound reports whether the error means the requested object does not exist.
func isNotFound(err error) bool {
	return errdefs.IsNotFound(err)
}

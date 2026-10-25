// Copyright 2026 Chun Huang (Charles).

// This Source Code Form is dual-licensed.
// By default, this file is licensed under the GNU Affero General Public License v3.0.
// If you have a separate written commercial agreement, you may use this file under those terms instead.

package main

import (
	"bytes"
	"context"
	"errors"
	"io"
	"strings"
	"testing"

	"github.com/docker/docker/api/types/container"
	"github.com/docker/docker/api/types/mount"
	"github.com/docker/docker/api/types/network"
	"github.com/docker/docker/api/types/volume"
	"github.com/docker/docker/errdefs"
	"github.com/docker/docker/pkg/stdcopy"
	"github.com/honeydipper/honeydipper/v4/pkg/dipper"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// fakeJobAPI is an in-memory jobAPI used to exercise the lifecycle, exec,
// volume and log-tail logic.
type fakeJobAPI struct {
	states     map[string]containerState
	waitCodes  map[string]int64
	ops        []string
	listResult []jobContainer
	listErr    error
	inspectErr map[string]error
	removeErr  map[string]error
	volumes    map[string]bool
	// logStreams keyed by container ID: raw stream bytes returned by
	// ContainerLogs. Stored as bytes so each call returns a fresh reader (the
	// handler re-fetches the same container during the done-phase re-chunk).
	logStreams map[string][]byte
	// logOpts records the LogsOptions passed to ContainerLogs per container.
	logOpts map[string]container.LogsOptions
	// execIDs maps containerID -> exec ID to return from ExecCreate.
	execIDs map[string]string
	// execStreams maps execID -> stream returned from ExecAttach.
	execStreams map[string]io.ReadCloser
	// execInspects maps execID -> inspect result returned from ExecInspect.
	execInspects map[string]container.ExecInspect
	// removeVolumeForce records the force flag per volume name.
	removeVolumeForce map[string]bool
	// createdVolumes records the volume.CreateOptions passed to CreateVolume.
	createdVolumes []volume.CreateOptions
}

func newFakeJobAPI() *fakeJobAPI {
	return &fakeJobAPI{
		states:     map[string]containerState{},
		waitCodes:  map[string]int64{},
		inspectErr: map[string]error{},
		removeErr:  map[string]error{},
		volumes:    map[string]bool{},
	}
}

func (f *fakeJobAPI) record(op string) {
	f.ops = append(f.ops, op)
}

func (f *fakeJobAPI) ImageExists(ctx context.Context, ref string) (bool, error) {
	return true, nil
}

func (f *fakeJobAPI) PullImage(ctx context.Context, ref string) error {
	f.record("pull:" + ref)

	return nil
}

func (f *fakeJobAPI) CreateContainer(ctx context.Context, name string, cfg *container.Config, host *container.HostConfig, net *network.NetworkingConfig) (string, error) {
	f.record("create:" + name)
	f.states["id-"+name] = containerState{ID: "id-" + name, Name: name, Status: container.StateCreated}

	return "id-" + name, nil
}

func (f *fakeJobAPI) StartContainer(ctx context.Context, id string) error {
	f.record("start:" + id)
	st := f.states[id]
	st.Status = container.StateRunning
	st.Running = true
	f.states[id] = st

	return nil
}

func (f *fakeJobAPI) WaitContainer(ctx context.Context, id string) (int64, error) {
	f.record("wait:" + id)
	code := f.waitCodes[id]
	st := f.states[id]
	st.Status = container.StateExited
	st.Running = false
	st.ExitCode = int(code)
	f.states[id] = st

	return code, nil
}

func (f *fakeJobAPI) StopContainer(ctx context.Context, id string) error {
	f.record("stop:" + id)
	st := f.states[id]
	st.Status = container.StateExited
	st.Running = false
	f.states[id] = st

	return nil
}

func (f *fakeJobAPI) InspectContainer(ctx context.Context, id string) (containerState, error) {
	f.record("inspect:" + id)
	if err, ok := f.inspectErr[id]; ok {
		return containerState{}, err
	}

	return f.states[id], nil
}

func (f *fakeJobAPI) RemoveContainer(ctx context.Context, id string, force bool) error {
	f.record("remove:" + id)
	if err, ok := f.removeErr[id]; ok {
		return err
	}

	return nil
}

func (f *fakeJobAPI) ListContainers(ctx context.Context, jobID string) ([]jobContainer, error) {
	f.record("list:" + jobID)
	if f.listErr != nil {
		return nil, f.listErr
	}

	return f.listResult, nil
}

func (f *fakeJobAPI) ContainerLogs(ctx context.Context, id string, opts container.LogsOptions) (io.ReadCloser, error) {
	f.record("logs:" + id)
	if f.logOpts == nil {
		f.logOpts = map[string]container.LogsOptions{}
	}
	f.logOpts[id] = opts
	if f.logStreams != nil {
		if data, ok := f.logStreams[id]; ok {
			return io.NopCloser(bytes.NewReader(data)), nil
		}
	}

	return io.NopCloser(bytes.NewReader(nil)), nil
}

func (f *fakeJobAPI) ExecCreate(ctx context.Context, containerID string, opts container.ExecOptions) (string, error) {
	f.record("execCreate:" + containerID)
	if f.execIDs != nil {
		if id, ok := f.execIDs[containerID]; ok {
			return id, nil
		}
	}

	return "exec-" + containerID, nil
}

func (f *fakeJobAPI) ExecAttach(ctx context.Context, execID string, opts container.ExecAttachOptions) (io.ReadCloser, error) {
	f.record("execAttach:" + execID)
	if f.execStreams != nil {
		if rc, ok := f.execStreams[execID]; ok {
			return rc, nil
		}
	}

	return io.NopCloser(bytes.NewReader(nil)), nil
}

func (f *fakeJobAPI) ExecInspect(ctx context.Context, execID string) (container.ExecInspect, error) {
	f.record("execInspect:" + execID)
	if f.execInspects != nil {
		if ins, ok := f.execInspects[execID]; ok {
			return ins, nil
		}
	}

	return container.ExecInspect{Running: false, ExitCode: 0}, nil
}

func (f *fakeJobAPI) VolumeExists(ctx context.Context, name string) (bool, error) {
	f.record("volumeExists:" + name)

	return f.volumes[name], nil
}

func (f *fakeJobAPI) CreateVolume(ctx context.Context, opts volume.CreateOptions) error {
	f.record("createVolume:" + opts.Name)
	f.createdVolumes = append(f.createdVolumes, opts)
	if f.volumes == nil {
		f.volumes = map[string]bool{}
	}
	f.volumes[opts.Name] = true

	return nil
}

func (f *fakeJobAPI) RemoveVolume(ctx context.Context, name string, force bool) error {
	f.record("removeVolume:" + name)
	if f.removeVolumeForce == nil {
		f.removeVolumeForce = map[string]bool{}
	}
	f.removeVolumeForce[name] = force

	return nil
}

func (f *fakeJobAPI) hasOp(op string) bool {
	for _, o := range f.ops {
		if o == op {
			return true
		}
	}

	return false
}

// --- spec parsing -----------------------------------------------------------

func TestParseContainerSpec(t *testing.T) {
	raw := map[string]any{
		"name":            "app",
		"image":           "busybox",
		"imagePullPolicy": "Always",
		"command":         []any{"sh", "-c", "echo hi"},
		"entrypoint":      []any{"/bin/entry"},
		"workdir":         "/work",
		"user":            "1000:1000",
		"env":             map[string]any{"FOO": "bar", "BAZ": "1"},
		"labels":          map[string]any{"team": "platform"},
		"tty":             true,
		"privileged":      true,
		"network":         "bridge",
		"binds":           []any{"/tmp/x:/tmp/x"},
		"memory":          100,
		"cpus":            0.5,
	}
	spec, err := parseContainerSpec(raw)
	require.NoError(t, err)
	assert.Equal(t, "app", spec.Name)
	assert.Equal(t, "busybox", spec.Image)
	assert.Equal(t, "Always", spec.ImagePullPolicy)
	assert.Equal(t, []string{"sh", "-c", "echo hi"}, []string(spec.Config.Cmd))
	assert.Equal(t, []string{"/bin/entry"}, []string(spec.Config.Entrypoint))
	assert.Equal(t, "/work", spec.Config.WorkingDir)
	assert.Equal(t, "1000:1000", spec.Config.User)
	assert.ElementsMatch(t, []string{"FOO=bar", "BAZ=1"}, spec.Config.Env)
	assert.Equal(t, "platform", spec.Config.Labels["team"])
	assert.True(t, spec.Config.Tty)
	assert.True(t, spec.HostConfig.Privileged)
	assert.Equal(t, container.NetworkMode("bridge"), spec.HostConfig.NetworkMode)
	assert.Equal(t, []string{"/tmp/x:/tmp/x"}, spec.HostConfig.Binds)
	assert.Equal(t, int64(100), spec.HostConfig.Memory)
	assert.Equal(t, int64(500000000), spec.HostConfig.NanoCPUs)
}

func TestParseContainerSpecMissingImage(t *testing.T) {
	_, err := parseContainerSpec(map[string]any{"name": "app"})
	assert.Error(t, err)
}

func TestParseContainerSpecEnvList(t *testing.T) {
	spec, err := parseContainerSpec(map[string]any{
		"image": "busybox",
		"env":   []any{"A=1", "B=2"},
	})
	require.NoError(t, err)
	assert.ElementsMatch(t, []string{"A=1", "B=2"}, spec.Config.Env)
}

func TestParseContainerSpecMounts(t *testing.T) {
	spec, err := parseContainerSpec(map[string]any{
		"image": "busybox",
		"mounts": []any{
			map[string]any{"type": "volume", "source": "vol", "target": "/data", "read_only": true},
		},
	})
	require.NoError(t, err)
	require.Len(t, spec.HostConfig.Mounts, 1)
	assert.Equal(t, mount.TypeVolume, spec.HostConfig.Mounts[0].Type)
	assert.Equal(t, "vol", spec.HostConfig.Mounts[0].Source)
	assert.Equal(t, "/data", spec.HostConfig.Mounts[0].Target)
	assert.True(t, spec.HostConfig.Mounts[0].ReadOnly)
}

// --- resolveRoles -----------------------------------------------------------

func TestResolveRoles(t *testing.T) {
	containers := []any{
		map[string]any{"name": "c0", "image": "img"},
		map[string]any{"name": "c1", "image": "img"},
		map[string]any{"name": "c2", "image": "img"},
	}
	roles := resolveRoles(containers)
	assert.Equal(t, []int{0, 1}, roles.init)
	assert.Equal(t, 2, roles.main)
}

func TestResolveRolesSingleContainer(t *testing.T) {
	roles := resolveRoles([]any{map[string]any{"name": "only", "image": "img"}})
	assert.Empty(t, roles.init)
	assert.Equal(t, 0, roles.main)
}

// --- sequencing state machine ------------------------------------------------

func TestRunJobSequenceInitSuccessThenMain(t *testing.T) {
	api := newFakeJobAPI()
	init0 := jobContainer{ID: "init0", Name: "init0"}
	init1 := jobContainer{ID: "init1", Name: "init1"}
	main := jobContainer{ID: "main", Name: "main"}
	api.states["init0"] = containerState{ID: "init0", Status: container.StateCreated}
	api.states["init1"] = containerState{ID: "init1", Status: container.StateCreated}
	api.states["main"] = containerState{ID: "main", Status: container.StateCreated}
	api.waitCodes["init0"] = 0
	api.waitCodes["init1"] = 0

	outcome, err := runJobSequence(context.Background(), api, []jobContainer{init0, init1}, main)
	require.NoError(t, err)
	assert.True(t, outcome.succeeded)
	// init0 and init1 each: inspect -> start -> wait, then main: inspect -> start
	assert.Equal(t, []string{
		"inspect:init0", "start:init0", "wait:init0",
		"inspect:init1", "start:init1", "wait:init1",
		"inspect:main", "start:main",
	}, api.ops)
}

func TestRunJobSequenceInitFailureSkipsMain(t *testing.T) {
	api := newFakeJobAPI()
	init0 := jobContainer{ID: "init0", Name: "init0"}
	init1 := jobContainer{ID: "init1", Name: "init1"}
	main := jobContainer{ID: "main", Name: "main"}
	api.states["init0"] = containerState{ID: "init0", Status: container.StateCreated}
	api.states["init1"] = containerState{ID: "init1", Status: container.StateCreated}
	api.waitCodes["init0"] = 0
	api.waitCodes["init1"] = 3

	outcome, err := runJobSequence(context.Background(), api, []jobContainer{init0, init1}, main)
	require.NoError(t, err)
	assert.False(t, outcome.succeeded)
	assert.Equal(t, "init1", outcome.initFailed)
	assert.Equal(t, 3, outcome.initExitCode)
	assert.False(t, api.hasOp("start:main"), "main must not be started when an init fails")
}

func TestRunJobSequenceResumeAlreadyStartedAndExited(t *testing.T) {
	api := newFakeJobAPI()
	init0 := jobContainer{ID: "init0", Name: "init0"}
	init1 := jobContainer{ID: "init1", Name: "init1"}
	main := jobContainer{ID: "main", Name: "main"}
	// init0 was started before the command was interrupted and is still running.
	api.states["init0"] = containerState{ID: "init0", Status: container.StateRunning, Running: true}
	// init1 already exited with success before start_job was re-queued.
	api.states["init1"] = containerState{ID: "init1", Status: container.StateExited, ExitCode: 0}
	// main was already started.
	api.states["main"] = containerState{ID: "main", Status: container.StateRunning, Running: true}
	api.waitCodes["init0"] = 0

	outcome, err := runJobSequence(context.Background(), api, []jobContainer{init0, init1}, main)
	require.NoError(t, err)
	assert.True(t, outcome.succeeded)
	// init0: inspected then waited on (NOT started again)
	assert.True(t, api.hasOp("wait:init0"))
	assert.False(t, api.hasOp("start:init0"), "already-running init must not be started again")
	// init1: inspected, exit code adopted, NOT started or waited on
	assert.False(t, api.hasOp("start:init1"))
	assert.False(t, api.hasOp("wait:init1"))
	// main: inspected, already running, NOT started again
	assert.False(t, api.hasOp("start:main"))
}

func TestRunJobSequencePreExitedInitAdoptsExitCode(t *testing.T) {
	api := newFakeJobAPI()
	init0 := jobContainer{ID: "init0", Name: "init0"}
	main := jobContainer{ID: "main", Name: "main"}
	// init0 already exited with a failure before start_job ran.
	api.states["init0"] = containerState{ID: "init0", Status: container.StateExited, ExitCode: 7}
	api.states["main"] = containerState{ID: "main", Status: container.StateCreated}

	outcome, err := runJobSequence(context.Background(), api, []jobContainer{init0}, main)
	require.NoError(t, err)
	assert.False(t, outcome.succeeded)
	assert.Equal(t, "init0", outcome.initFailed)
	assert.Equal(t, 7, outcome.initExitCode)
	assert.False(t, api.hasOp("start:init0"), "exited init must not be started")
	assert.False(t, api.hasOp("start:main"), "main must be skipped")
}

// --- fatalSetStatus -----------------------------------------------------------

func TestFatalSetStatus(t *testing.T) {
	roles := roles{init: []int{0, 1}, main: 2}
	containers := []containerState{
		{Name: "init0", Index: 0, Status: container.StateExited, ExitCode: 0},
		{Name: "init1", Index: 1, Status: container.StateExited, ExitCode: 0},
		{Name: "main", Index: 2, Status: container.StateExited, ExitCode: 0},
	}
	done, status, reason := fatalSetStatus(containers, roles)
	assert.True(t, done)
	assert.Equal(t, dipper.SUCCESS, status)
	assert.Empty(t, reason)
}

func TestFatalSetStatusRunningFatalNotDone(t *testing.T) {
	roles := roles{init: []int{0}, main: 1}
	containers := []containerState{
		{Name: "init0", Index: 0, Status: container.StateExited, ExitCode: 0},
		{Name: "main", Index: 1, Status: container.StateRunning, Running: true},
	}
	done, status, _ := fatalSetStatus(containers, roles)
	assert.False(t, done)
	assert.Equal(t, dipper.SUCCESS, status)
}

func TestFatalSetStatusFatalNonZero(t *testing.T) {
	roles := roles{init: []int{0}, main: 1}
	containers := []containerState{
		{Name: "init0", Index: 0, Status: container.StateExited, ExitCode: 2},
		{Name: "main", Index: 1, Status: container.StateExited, ExitCode: 0},
	}
	done, status, reason := fatalSetStatus(containers, roles)
	assert.True(t, done)
	assert.Equal(t, dipper.FAILURE, status)
	assert.Contains(t, reason, "init0")
	assert.Contains(t, reason, "2")
}

func TestFatalSetStatusNonFatalDoesNotBlockOrFlip(t *testing.T) {
	roles := roles{init: []int{0}, main: 1}
	containers := []containerState{
		{Name: "init0", Index: 0, Status: container.StateExited, ExitCode: 0},
		{Name: "main", Index: 1, Status: container.StateExited, ExitCode: 0},
		// A future sidecar (non-fatal): running with a non-zero exit — must not
		// affect done or status.
		{Name: "sidecar", Index: 9, Status: container.StateRunning, Running: true, ExitCode: 1},
	}
	done, status, reason := fatalSetStatus(containers, roles)
	assert.True(t, done, "a running non-fatal container must not block done")
	assert.Equal(t, dipper.SUCCESS, status, "a non-fatal non-zero exit must not flip status")
	assert.Empty(t, reason)
}

// --- volume ownership and validation -----------------------------------------

func TestSanitizeVolumeName(t *testing.T) {
	cases := []struct {
		in   string
		want string
	}{
		{"hd-sess-1-work", "hd-sess-1-work"},
		{"hd-sess/1-work", "hd-sess-1-work"},
		{"hd-sess:1:work", "hd-sess-1-work"},
		{"1hd-work", "1hd-work"},
		{"-hd-work", "vhd-work"},
	}
	for _, c := range cases {
		got, err := sanitizeVolumeName(c.in)
		require.NoError(t, err)
		assert.Equal(t, c.want, got)
		assert.True(t, validVolumeName(got), "sanitized name must be a valid docker volume name")
	}
}

func TestSanitizeVolumeNameEmpty(t *testing.T) {
	_, err := sanitizeVolumeName("")
	assert.Error(t, err)
}

// --- delete_on_success ---------------------------------------------------------

func TestDeleteOnSuccessOnlyOnSuccess(t *testing.T) {
	job := "hd-sess-1"

	api := newFakeJobAPI()
	api.listResult = []jobContainer{{ID: "main", Name: "main", Role: string(roleMain), Index: 0}}
	api.states["main"] = containerState{ID: "main", Name: "main", Index: 0, Status: container.StateExited, ExitCode: 0}

	_, labels, err := fetchJobLog(context.Background(), api, job, true)
	require.NoError(t, err)
	assert.Equal(t, dipper.SUCCESS, labels["status"])
	assert.True(t, api.hasOp("remove:main"), "delete_on_success must clean up on success")

	// failure case: no cleanup
	api2 := newFakeJobAPI()
	api2.listResult = []jobContainer{{ID: "main", Name: "main", Role: string(roleMain), Index: 0}}
	api2.states["main"] = containerState{ID: "main", Name: "main", Index: 0, Status: container.StateExited, ExitCode: 1}
	_, labels, err = fetchJobLog(context.Background(), api2, job, true)
	require.NoError(t, err)
	assert.Equal(t, dipper.FAILURE, labels["status"])
	assert.False(t, api2.hasOp("remove:main"), "delete_on_success must NOT clean up on failure")

	// no cleanup when delete_on_success is false
	api3 := newFakeJobAPI()
	api3.listResult = []jobContainer{{ID: "main", Name: "main", Role: string(roleMain), Index: 0}}
	api3.states["main"] = containerState{ID: "main", Name: "main", Index: 0, Status: container.StateExited, ExitCode: 0}
	_, _, err = fetchJobLog(context.Background(), api3, job, false)
	require.NoError(t, err)
	assert.False(t, api3.hasOp("remove:main"))
}

func TestDeleteOnSuccessSkipsStillRunningJob(t *testing.T) {
	// A mid-run get_job_log sees a running main: fatalSetStatus reports success
	// (running does not flip status), so cleanup must be gated on done==true too
	// — otherwise delete_on_success would stop and remove the running main.
	api := newFakeJobAPI()
	api.listResult = []jobContainer{{ID: "main", Name: "main", Role: string(roleMain), Index: 0}}
	api.states["main"] = containerState{ID: "main", Name: "main", Index: 0, Status: container.StateRunning, Running: true}

	_, labels, err := fetchJobLog(context.Background(), api, "hd-sess-1", true)
	require.NoError(t, err)
	assert.Equal(t, dipper.SUCCESS, labels["status"])
	assert.False(t, api.hasOp("remove:main"), "delete_on_success must not clean up a still-running job")
}

// --- delete_job ----------------------------------------------------------------

func TestDeleteJobIdempotentOnNotFound(t *testing.T) {
	api := newFakeJobAPI()
	api.listErr = errdefs.NotFound(errors.New("no such container"))
	err := removeJob(context.Background(), api, "hd-sess-1")
	assert.NoError(t, err)

	api2 := newFakeJobAPI() // empty list, no containers match the label
	err = removeJob(context.Background(), api2, "hd-sess-1")
	assert.NoError(t, err)
}

func TestDeleteJobRemovesContainersBeforeVolume(t *testing.T) {
	api := newFakeJobAPI()
	api.listResult = []jobContainer{
		{ID: "init0", Name: "init0", Role: string(roleInit), Index: 0, WorkVolume: "hd-sess-1-work", WorkVolumeOwned: true},
		{ID: "main", Name: "main", Role: string(roleMain), Index: 1, WorkVolume: "hd-sess-1-work", WorkVolumeOwned: true},
	}
	api.states["init0"] = containerState{ID: "init0", Status: container.StateRunning, Running: true}
	api.states["main"] = containerState{ID: "main", Status: container.StateCreated}

	err := removeJob(context.Background(), api, "hd-sess-1")
	require.NoError(t, err)

	// both containers removed
	assert.True(t, api.hasOp("remove:init0"))
	assert.True(t, api.hasOp("remove:main"))
	// running container was stopped first
	stopIdx := indexOfOp(api.ops, "stop:init0")
	removeIdx := indexOfOp(api.ops, "remove:init0")
	require.True(t, stopIdx >= 0 && removeIdx >= 0)
	assert.True(t, stopIdx < removeIdx)
	// volume removed AFTER all containers
	volIdx := indexOfOp(api.ops, "removeVolume:hd-sess-1-work")
	lastRemove := -1
	for i, op := range api.ops {
		if len(op) > 7 && op[:7] == "remove:" {
			lastRemove = i
		}
	}
	require.True(t, volIdx >= 0)
	assert.True(t, volIdx > lastRemove, "volume must be removed after all containers")
}

func TestDeleteJobDoesNotRemovePreExistingVolume(t *testing.T) {
	api := newFakeJobAPI()
	api.listResult = []jobContainer{
		{ID: "main", Name: "main", Role: string(roleMain), Index: 0, WorkVolume: "my-persistent-vol", WorkVolumeOwned: false},
	}
	api.states["main"] = containerState{ID: "main", Status: container.StateExited, ExitCode: 0}
	err := removeJob(context.Background(), api, "hd-sess-1")
	require.NoError(t, err)
	assert.False(t, api.hasOp("removeVolume:my-persistent-vol"), "pre-existing volume must never be removed")
}

// --- name suffix ---------------------------------------------------------------

func TestJobNaming(t *testing.T) {
	assert.Equal(t, "-hd-sess-3", jobSuffix("sess", "3"))
	assert.Equal(t, "hd-sess-3", jobID("sess", "3"))
	assert.Equal(t, "app-hd-sess-3-c", jobContainerName("app", "sess", "3"))
	assert.Equal(t, "hd-init-0-hd-sess-3-c", jobContainerName("hd-init-0", "sess", "3"))
}

// --- image pull stream error decoding ------------------------------------------

func TestPullStreamErrorSuccess(t *testing.T) {
	stream := strings.Join([]string{
		`{"status":"Pulling from library/busybox"}`,
		`{"status":"Digest: sha256:abcdef123456"}`,
		`{"status":"Status: Downloaded newer image for busybox:latest"}`,
	}, "\n") + "\n"
	assert.NoError(t, pullStreamError(strings.NewReader(stream), "busybox"))
}

func TestPullStreamErrorSurfacesStreamError(t *testing.T) {
	stream := `{"errorDetail":{"message":"pull access denied for missing/repo, repository does not exist or may require 'docker login': denied: requested access to the resource is denied"},"error":"pull access denied for missing/repo, repository does not exist or may require 'docker login': denied: requested access to the resource is denied"}` + "\n"
	err := pullStreamError(strings.NewReader(stream), "missing/repo")
	require.Error(t, err)
	assert.Contains(t, err.Error(), "missing/repo")
	assert.Contains(t, err.Error(), "pull access denied")
}

func TestPullStreamErrorSurfacesTopLevelErrorField(t *testing.T) {
	// Some failures arrive only in the deprecated top-level `error` field.
	stream := `{"status":"Pulling from missing/repo","error":"manifest unknown"}` + "\n"
	err := pullStreamError(strings.NewReader(stream), "missing/repo")
	require.Error(t, err)
	assert.Contains(t, err.Error(), "manifest unknown")
}

// --- wait_job handler-level tests ------------------------------------------------

// newTestDriver builds a driver whose jobAPI is a fake, so handler-level tests
// can exercise the full command path (payload -> reply) without a Docker daemon.
func newTestDriver(api jobAPI) *dockerDriver {
	d := &dockerDriver{Driver: dipper.NewDriver("test", "docker")}
	d.jobAPIFor = func(ctx context.Context, _ *dipper.Message) (context.Context, jobAPI) {
		return ctx, api
	}

	return d
}

// waitJobMessage is a helper to invoke the wait_job handler with a reply channel.
func waitJobMessage(t *testing.T, d *dockerDriver) dipper.Message {
	t.Helper()
	reply := make(chan dipper.Message, 1)
	d.waitJob(&dipper.Message{
		Payload: map[string]any{"pod_id": "hd-sess-1"},
		Labels:  map[string]string{"sessionID": "sess", "cursor": "1"},
		Reply:   reply,
	})

	return <-reply
}

func TestWaitJobPreExitedInitFailureRepliesImmediately(t *testing.T) {
	api := newFakeJobAPI()
	d := newTestDriver(api)
	api.listResult = []jobContainer{
		{ID: "init0", Name: "init0", Role: string(roleInit), Index: 0},
		{ID: "main", Name: "main", Role: string(roleMain), Index: 1},
	}
	// init0 already failed; start_job skipped main, which is still "created".
	api.states["init0"] = containerState{ID: "init0", Status: container.StateExited, ExitCode: 2}
	api.states["main"] = containerState{ID: "main", Status: container.StateCreated}

	got := waitJobMessage(t, d)
	assert.Equal(t, dipper.FAILURE, got.Labels["status"])
	assert.Contains(t, got.Labels["reason"], "init0")
	// Must NOT wait on the never-started main: Docker's Wait on a "created"
	// container returns immediately with exit code 0 (the Wait condition is
	// already met), which would report the failed job as success; the pre-wait
	// aggregation makes the failure explicit and immediate instead.
	assert.False(t, api.hasOp("wait:main"), "must not wait on a never-started main")
}

func TestWaitJobSuccessWhenMainAlreadyTerminal(t *testing.T) {
	api := newFakeJobAPI()
	d := newTestDriver(api)
	api.listResult = []jobContainer{
		{ID: "init0", Name: "init0", Role: string(roleInit), Index: 0},
		{ID: "main", Name: "main", Role: string(roleMain), Index: 1},
	}
	api.states["init0"] = containerState{ID: "init0", Status: container.StateExited, ExitCode: 0}
	api.states["main"] = containerState{ID: "main", Status: container.StateExited, ExitCode: 0}

	got := waitJobMessage(t, d)
	assert.Equal(t, dipper.SUCCESS, got.Labels["status"])
	assert.False(t, api.hasOp("wait:main"), "already-terminal main must not be waited on")
}

func TestWaitJobWaitsForRunningMain(t *testing.T) {
	api := newFakeJobAPI()
	d := newTestDriver(api)
	api.listResult = []jobContainer{
		{ID: "init0", Name: "init0", Role: string(roleInit), Index: 0},
		{ID: "main", Name: "main", Role: string(roleMain), Index: 1},
	}
	api.states["init0"] = containerState{ID: "init0", Status: container.StateExited, ExitCode: 0}
	api.states["main"] = containerState{ID: "main", Status: container.StateRunning, Running: true}
	api.waitCodes["main"] = 0

	got := waitJobMessage(t, d)
	assert.Equal(t, dipper.SUCCESS, got.Labels["status"])
	assert.True(t, api.hasOp("wait:main"), "a running main must be waited on")
}

// --- stdcopy frame decoding -----------------------------------------------------

func TestDecodeJobLogStdCopy(t *testing.T) {
	var framed bytes.Buffer
	stdout := stdcopy.NewStdWriter(&framed, stdcopy.Stdout)
	stderr := stdcopy.NewStdWriter(&framed, stdcopy.Stderr)
	_, _ = stdout.Write([]byte("out-line-1\n"))
	_, _ = stderr.Write([]byte("err-line-1\n"))
	_, _ = stdout.Write([]byte("out-line-2\n"))

	text, err := decodeJobLog(&framed, false)
	require.NoError(t, err)
	assert.Equal(t, "out-line-1\nerr-line-1\nout-line-2\n", text)
}

func TestDecodeJobLogTTY(t *testing.T) {
	// A TTY container's log stream is raw (not 8-byte-framed); running it
	// through stdcopy would misinterpret payload bytes as frame headers, so it
	// must be read verbatim.
	raw := "line-1\nline-2\nline-3\n"
	text, err := decodeJobLog(strings.NewReader(raw), true)
	require.NoError(t, err)
	assert.Equal(t, raw, text)
}

func TestDecodeJobLogTTYNonAsciiRaw(t *testing.T) {
	// Payload bytes that happen to look like a stdcopy frame header (e.g. the
	// byte 0x01) must not be treated as framing when tty is true.
	raw := string([]byte{0x01, 0x02, 0x03, 'h', 'i', '\n'})
	text, err := decodeJobLog(strings.NewReader(raw), true)
	require.NoError(t, err)
	assert.Equal(t, raw, text)
}

// --- helpers -------------------------------------------------------------------

func indexOfOp(ops []string, want string) int {
	for i, op := range ops {
		if op == want {
			return i
		}
	}

	return -1
}

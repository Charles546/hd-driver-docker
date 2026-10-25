// Copyright 2026 Chun Huang (Charles).

// This Source Code Form is dual-licensed.
// By default, this file is licensed under the GNU Affero General Public License v3.0.
// If you have a separate written commercial agreement, you may use this file under those terms instead.

package main

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"regexp"
	"sort"
	"strconv"
	"strings"

	"github.com/docker/docker/api/types/container"
	"github.com/docker/docker/api/types/mount"
	"github.com/docker/docker/api/types/volume"
	"github.com/docker/docker/pkg/stdcopy"
	"github.com/honeydipper/honeydipper/v4/pkg/dipper"
)

// volumeNamePattern is the charset Docker allows for volume names.
var volumeNamePattern = regexp.MustCompile(`^[a-zA-Z0-9][a-zA-Z0-9_.-]*$`)

// jobSuffix returns the `-hd-<sessionID>-<cursor>` suffix applied to the names
// of the resources (containers) belonging to a job.
func jobSuffix(sessionID, cursor string) string {
	return fmt.Sprintf("-hd-%s-%s", sessionID, cursor)
}

// jobID returns the stable identifier of a job, derived from the session and
// cursor. It is used as the `pod_id` reply value and as the `hd-job` label on
// every resource the driver creates for the job.
func jobID(sessionID, cursor string) string {
	return fmt.Sprintf("hd-%s-%s", sessionID, cursor)
}

// jobContainerName returns the Docker container name for a job container. The
// base name comes from the container spec (or a generated name when the spec
// has none); the `-hd-<sessionID>-<cursor>-c` suffix makes it unique per job.
func jobContainerName(base, sessionID, cursor string) string {
	return base + jobSuffix(sessionID, cursor) + "-c"
}

// isAlnum reports whether r is an ASCII alphanumeric character.
func isAlnum(r rune) bool {
	return r >= 'a' && r <= 'z' || r >= 'A' && r <= 'Z' || r >= '0' && r <= '9'
}

// isVolumeNameChar reports whether r is allowed inside a Docker volume name
// (after the required leading alphanumeric).
func isVolumeNameChar(r rune) bool {
	return isAlnum(r) || r == '_' || r == '.' || r == '-'
}

// sanitizeVolumeName validates a driver-derived volume name against Docker's
// volume-name charset `[a-zA-Z0-9][a-zA-Z0-9_.-]*` and sanitizes it by replacing
// invalid characters with `-` (forcing a valid leading alphanumeric character).
func sanitizeVolumeName(name string) (string, error) {
	if name == "" {
		return "", fmt.Errorf("volume name cannot be empty")
	}
	var b strings.Builder
	for i, r := range name {
		if !isVolumeNameChar(r) {
			r = '-'
		}
		if i == 0 && !isAlnum(r) {
			r = 'v'
		}
		b.WriteRune(r)
	}
	ret := b.String()
	if !volumeNamePattern.MatchString(ret) {
		return "", fmt.Errorf("sanitized volume name %q is not a valid docker volume name", ret)
	}

	return ret, nil
}

// validVolumeName reports whether the name already conforms to Docker's
// volume-name charset.
func validVolumeName(name string) bool {
	return volumeNamePattern.MatchString(name)
}

// containerRole is the role of a job container.
type containerRole string

const (
	roleInit containerRole = "init"
	roleMain containerRole = "main"
)

// roles assigns a role to every container index of a job. In Phase 1 the rules
// are fixed: all but the last container are init containers and the last is the
// main container. resolveRoles is the ONLY place this "last container is main"
// rule is encoded, so a future sidecar extension (concurrent non-fatal
// containers selected by an optional `mode` field) is a data-only change here.
type roles struct {
	init []int // indices of init containers, in order
	main int   // index of the main container (-1 when there is none)
}

// resolveRoles maps the ordered flat container list to role assignments.
func resolveRoles(containers []any) roles {
	ret := roles{main: -1}
	for i := range containers {
		if i == len(containers)-1 {
			ret.main = i
		} else {
			ret.init = append(ret.init, i)
		}
	}

	return ret
}

// containerState is the observed state of one job container relevant to
// sequencing and status aggregation.
type containerState struct {
	ID       string
	Name     string
	Index    int
	Status   string // one of container.State* (created/running/exited/...)
	ExitCode int
	Running  bool
	// Tty records whether the container was created with a TTY, which changes
	// how its Docker log stream is encoded (see decodeJobLog).
	Tty bool
}

// isTerminalState reports whether a container has reached a terminal state.
func isTerminalState(status string) bool {
	return status == container.StateExited || status == container.StateDead
}

// fatalSetStatus aggregates the FATAL set (init + main) of a job only. done is
// true when every fatal-set container reached a terminal state. status is
// "failure" (with a reason naming the first offending container) iff any fatal
// container exited with a non-zero exit code. Containers outside the fatal set
// (future sidecars) never influence done or status. It is written once and used
// by BOTH wait_job and the Phase 2 log-tail container-state builder.
func fatalSetStatus(containers []containerState, roles roles) (done bool, status string, reason string) {
	fatal := map[int]bool{}
	for _, i := range roles.init {
		fatal[i] = true
	}
	if roles.main >= 0 {
		fatal[roles.main] = true
	}
	done = true
	status = dipper.SUCCESS
	for _, c := range containers {
		if !fatal[c.Index] {
			continue
		}
		if !isTerminalState(c.Status) {
			done = false
		} else if c.ExitCode != 0 {
			status = dipper.FAILURE
			if reason == "" {
				reason = fmt.Sprintf("container %s failed with exit code %d", c.Name, c.ExitCode)
			}
		}
	}

	return done, status, reason
}

// sequenceOutcome is the result of running the init→main sequencing state machine.
type sequenceOutcome struct {
	// succeeded is true when all init containers passed and the main container
	// was started (or was already started / already exited).
	succeeded bool
	// initFailed is the name of the init container that failed (empty when
	// succeeded).
	initFailed string
	// initExitCode is the exit code of the failed init container.
	initExitCode int
}

// runJobSequence drives the Docker-native init-container sequencing state machine.
// It starts each init container one at a time and waits for it to exit before
// starting the next; if an init container fails it stops, reports which init
// container failed and its exit code, and skips the main container. Only after
// all init containers succeed does it start the main container.
//
// start_job is interruptible and the operator re-queues interrupted commands
// (up to 3 retries / restart recovery), so the sequence is resumable: an init
// container that is already running is waited on (continue the sequence), an
// init container that already exited has its exit code adopted without trying
// to start it again, and a main container that is already started or exited is
// not started twice.
func runJobSequence(ctx context.Context, api jobAPI, inits []jobContainer, main jobContainer) (sequenceOutcome, error) {
	outcome := sequenceOutcome{}
	for _, init := range inits {
		code, err := ensureInitDone(ctx, api, init.ID)
		if err != nil {
			return outcome, err
		}
		if code != 0 {
			outcome.succeeded = false
			outcome.initFailed = init.Name
			outcome.initExitCode = int(code)

			return outcome, nil
		}
	}
	// all init containers succeeded: start (or adopt) the main container.
	if err := ensureMainStarted(ctx, api, main.ID); err != nil {
		return outcome, err
	}
	outcome.succeeded = true

	return outcome, nil
}

// ensureInitDone brings one init container to a terminal state and returns its
// exit code, tolerating resume: an already-running container is waited on, and
// an already-exited container has its exit code adopted (it is not started).
func ensureInitDone(ctx context.Context, api jobAPI, id string) (int64, error) {
	state, err := api.InspectContainer(ctx, id)
	if err != nil {
		return 0, err
	}
	switch state.Status {
	case container.StateRunning, container.StatePaused, container.StateRestarting:
		return api.WaitContainer(ctx, id)
	case container.StateExited, container.StateDead:
		return int64(state.ExitCode), nil
	default:
		if err := api.StartContainer(ctx, id); err != nil {
			return 0, err
		}

		return api.WaitContainer(ctx, id)
	}
}

// ensureMainStarted starts the main container unless it is already started or
// already reached a terminal state (resume safety).
func ensureMainStarted(ctx context.Context, api jobAPI, id string) error {
	state, err := api.InspectContainer(ctx, id)
	if err != nil {
		return err
	}
	switch state.Status {
	case container.StateRunning, container.StatePaused, container.StateRestarting,
		container.StateExited, container.StateDead:
		return nil
	default:
		return api.StartContainer(ctx, id)
	}
}

// splitJobContainers partitions job containers into init and main.
func splitJobContainers(containers []jobContainer) (inits []jobContainer, main jobContainer, ok bool) {
	for _, c := range containers {
		if c.Role == string(roleMain) {
			main = c
			ok = true
		} else {
			inits = append(inits, c)
		}
	}

	return inits, main, ok
}

// rolesOf derives the fatal-set roles from the job containers found by label.
// The role assignments were recorded at create time by resolveRoles, so the
// "last container is main" rule is not re-encoded here.
func rolesOf(containers []jobContainer) roles {
	ret := roles{main: -1}
	for _, c := range containers {
		if c.Role == string(roleMain) {
			ret.main = c.Index
		} else {
			ret.init = append(ret.init, c.Index)
		}
	}

	return ret
}

// findJobContainers returns the containers belonging to a job, ordered init
// first then main (both in creation order). It relies on the labels set at
// create time, so it is restart-safe and requires no in-memory state.
func findJobContainers(ctx context.Context, api jobAPI, job string) ([]jobContainer, error) {
	containers, err := api.ListContainers(ctx, job)
	if err != nil {
		return nil, err
	}
	sort.SliceStable(containers, func(i, j int) bool {
		if containers[i].Role != containers[j].Role {
			return containers[i].Role == string(roleInit)
		}

		return containers[i].Index < containers[j].Index
	})

	return containers, nil
}

// inspectJobContainers inspects all of a job's containers and returns their
// states, tagging each with its job-derived name and index.
func inspectJobContainers(ctx context.Context, api jobAPI, containers []jobContainer) ([]containerState, error) {
	states := make([]containerState, 0, len(containers))
	for _, c := range containers {
		st, err := api.InspectContainer(ctx, c.ID)
		if err != nil {
			return nil, err
		}
		st.Name = c.Name
		st.Index = c.Index
		states = append(states, st)
	}

	return states, nil
}

// jobIDFromPayload extracts the job identifier from a message payload. It
// prefers the explicit `pod_id` (set by create_job and carried forward by the
// operator) and falls back to deriving it from the session/cursor labels.
func jobIDFromPayload(msg *dipper.Message) string {
	if job, ok := dipper.GetMapDataStr(msg.Payload, "pod_id"); ok && job != "" {
		return job
	}
	cursor := "0"
	if c, ok := msg.Labels["cursor"]; ok {
		cursor = c
	}

	return jobID(msg.Labels["sessionID"], cursor)
}

func (d *dockerDriver) createJob(msg *dipper.Message) {
	log := d.GetLogger()
	log.Debugf("[%s] create job with payload %+v", d.Service, msg.Payload)
	msg = dipper.DeserializePayload(msg)
	ctx, cancel := d.GetContext(msg)
	defer cancel()
	ctx, api := d.getJobAPI(ctx, msg)

	sessionID := msg.Labels["sessionID"]
	cursor := "0"
	if c, ok := msg.Labels["cursor"]; ok {
		cursor = c
	}
	job := jobID(sessionID, cursor)

	rawContainers := dipper.MustGetMapData(msg.Payload, "containers").([]any)
	if len(rawContainers) == 0 {
		panic(fmt.Errorf("job %s has no containers", job))
	}
	// resolveRoles is the forward-compat hook: the single place encoding which
	// containers are init and which is main.
	roles := resolveRoles(rawContainers)

	// Set up the shared work volume.
	workVolumeMountPoint := "/local/honeydipper"
	if v, ok := dipper.GetMapDataStr(msg.Payload, "work_volume_mount_point"); ok && v != "" {
		workVolumeMountPoint = v
	}
	useWorkVolume, _ := dipper.GetMapDataStr(msg.Payload, "use_work_volume")
	withWorkVolume, _ := dipper.GetMapDataBool(msg.Payload, "with_work_volume")
	var (
		workVolumeName  string
		workVolumeOwned bool
	)
	if useWorkVolume != "" {
		// Pre-existing named volume: never owned, never removed by cleanup.
		workVolumeName = useWorkVolume
	} else if withWorkVolume {
		// Driver-created ephemeral volume: sanitize the job-derived name.
		name, err := sanitizeVolumeName(job + "-work")
		if err != nil {
			panic(err)
		}
		exists, err := api.VolumeExists(ctx, name)
		if err != nil {
			panic(err)
		}
		if !exists {
			if err := api.CreateVolume(ctx, volume.CreateOptions{Name: name}); err != nil {
				panic(err)
			}
		}
		workVolumeName = name
		workVolumeOwned = true
	}

	containerIDs := map[string]string{}
	for i, raw := range rawContainers {
		role := roleInit
		if i == roles.main {
			role = roleMain
		}
		spec, err := parseContainerSpec(raw.(map[string]any))
		if err != nil {
			panic(err)
		}
		// Pull the image per imagePullPolicy (Always/Never/IfNotPresent;
		// default: pull if missing).
		exists, err := api.ImageExists(ctx, spec.Image)
		if err != nil {
			panic(err)
		}
		policy := strings.ToLower(spec.ImagePullPolicy)
		if !exists && policy != "never" || policy == "always" {
			if err := api.PullImage(ctx, spec.Image); err != nil {
				panic(err)
			}
		}
		// Deterministic container name: <base>-hd-<sessionID>-<cursor>-c.
		base := spec.Name
		if base == "" {
			base = fmt.Sprintf("hd-%s-%d", role, i)
		}
		name := jobContainerName(base, sessionID, cursor)
		if spec.Config.Labels == nil {
			spec.Config.Labels = map[string]string{}
		}
		spec.Config.Labels["hd-driver"] = "docker"
		spec.Config.Labels["hd-job"] = job
		spec.Config.Labels["hd-role"] = string(role)
		spec.Config.Labels["hd-index"] = strconv.Itoa(i)
		if workVolumeName != "" {
			spec.HostConfig.Mounts = append(spec.HostConfig.Mounts, mount.Mount{
				Type:   mount.TypeVolume,
				Source: workVolumeName,
				Target: workVolumeMountPoint,
			})
			spec.Config.Labels["hd-work-volume"] = workVolumeName
			spec.Config.Labels["hd-work-volume-owned"] = strconv.FormatBool(workVolumeOwned)
		}
		if spec.Config.WorkingDir == "" {
			spec.Config.WorkingDir = workVolumeMountPoint
		}
		id, err := api.CreateContainer(ctx, name, spec.Config, spec.HostConfig, spec.Networking)
		if err != nil {
			panic(err)
		}
		containerIDs[name] = id
	}

	log.Infof("created job %s with containers %v", job, containerIDs)
	msg.Reply <- dipper.Message{
		Payload: map[string]any{
			"pod_id":        job,
			"container_ids": containerIDs,
		},
	}
}

func (d *dockerDriver) startJob(msg *dipper.Message) {
	log := d.GetLogger()
	log.Debugf("[%s] start job with payload %+v", d.Service, msg.Payload)
	msg = dipper.DeserializePayload(msg)
	ctx, cancel := d.GetContext(msg)
	defer cancel()
	ctx, api := d.getJobAPI(ctx, msg)

	job := jobIDFromPayload(msg)
	containers, err := findJobContainers(ctx, api, job)
	if err != nil {
		panic(err)
	}
	inits, main, ok := splitJobContainers(containers)
	if !ok {
		panic(fmt.Errorf("job %s has no main container", job))
	}
	outcome, err := runJobSequence(ctx, api, inits, main)
	if err != nil {
		panic(err)
	}
	if !outcome.succeeded {
		msg.Reply <- dipper.Message{Labels: map[string]string{
			"status": dipper.FAILURE,
			"reason": fmt.Sprintf("init container %s failed with exit code %d", outcome.initFailed, outcome.initExitCode),
		}}

		return
	}
	msg.Reply <- dipper.Message{Labels: map[string]string{"status": dipper.SUCCESS}}
}

func (d *dockerDriver) waitJob(msg *dipper.Message) {
	log := d.GetLogger()
	log.Debugf("[%s] wait job with payload %+v", d.Service, msg.Payload)
	msg = dipper.DeserializePayload(msg)
	ctx, cancel := d.GetContext(msg)
	defer cancel()
	ctx, api := d.getJobAPI(ctx, msg)

	job := jobIDFromPayload(msg)
	containers, err := findJobContainers(ctx, api, job)
	if err != nil {
		panic(err)
	}
	roles := rolesOf(containers)

	// Aggregate over the CURRENT states before waiting. If any fatal-set
	// container is already terminal with a non-zero exit — e.g. an init
	// container failed and start_job skipped the main container, leaving it
	// "created" — the job is over and we reply failure immediately. Waiting on
	// a never-started (created) main returns immediately with exit code 0
	// (Docker's Wait condition is already met for a non-running container) and
	// would report a failed job as success; the pre-wait aggregation makes the
	// failure explicit and immediate instead.
	states, err := inspectJobContainers(ctx, api, containers)
	if err != nil {
		panic(err)
	}
	_, status, reason := fatalSetStatus(states, roles)
	if status == dipper.FAILURE {
		replyStatus(msg, status, reason)

		return
	}

	// Termination condition: the main container must reach a terminal state
	// (NOT "all containers terminal"). If it is already terminal, skip the wait.
	_, main, ok := splitJobContainers(containers)
	if !ok {
		panic(fmt.Errorf("job %s has no main container", job))
	}
	mainState, err := api.InspectContainer(ctx, main.ID)
	if err != nil {
		panic(err)
	}
	if !isTerminalState(mainState.Status) {
		if _, err := api.WaitContainer(ctx, main.ID); err != nil {
			panic(err)
		}
	}

	// Re-aggregate now that the main container is terminal.
	states, err = inspectJobContainers(ctx, api, containers)
	if err != nil {
		panic(err)
	}
	_, status, reason = fatalSetStatus(states, roles)

	replyStatus(msg, status, reason)
}

// replyStatus replies to a command message with a status and an optional reason.
func replyStatus(msg *dipper.Message, status, reason string) {
	ret := dipper.Message{Labels: map[string]string{"status": status}}
	if reason != "" {
		ret.Labels["reason"] = reason
	}
	msg.Reply <- ret
}

func (d *dockerDriver) getJobLog(msg *dipper.Message) {
	log := d.GetLogger()
	log.Debugf("[%s] get job log with payload %+v", d.Service, msg.Payload)
	msg = dipper.DeserializePayload(msg)
	ctx, cancel := d.GetContext(msg)
	defer cancel()
	ctx, api := d.getJobAPI(ctx, msg)

	deleteOnSuccess, _ := dipper.GetMapDataBool(msg.Payload, "delete_on_success")
	job := jobIDFromPayload(msg)
	payload, labels, err := fetchJobLog(ctx, api, job, deleteOnSuccess)
	if err != nil {
		panic(err)
	}
	if ce, ok := labels["cleanup_error"]; ok {
		// Return() only forwards status/reason labels, so a cleanup failure
		// recorded in labels would otherwise be dropped on the floor. Log it
		// and surface it to the caller in the payload.
		log.Warningf("[%s] cleanup of job %s failed: %s", d.Service, job, ce)
		payload["cleanup_error"] = ce
	}
	msg.Reply <- dipper.Message{Payload: payload, Labels: labels}
}

// fetchJobLog gathers the combined stdout/stderr of all of a job's containers
// (with timestamps) and applies the delete_on_success cleanup policy. It is
// separated from the command handler so the policy logic is unit-testable.
func fetchJobLog(ctx context.Context, api jobAPI, job string, deleteOnSuccess bool) (map[string]any, map[string]string, error) {
	containers, err := findJobContainers(ctx, api, job)
	if err != nil {
		return nil, nil, err
	}
	roles := rolesOf(containers)

	var all string
	perContainer := map[string]string{}
	states := []containerState{}
	for _, c := range containers {
		st, err := api.InspectContainer(ctx, c.ID)
		if err != nil {
			return nil, nil, err
		}
		st.Name = c.Name
		st.Index = c.Index
		states = append(states, st)

		rc, err := api.ContainerLogs(ctx, c.ID, container.LogsOptions{
			ShowStdout: true,
			ShowStderr: true,
			Timestamps: true,
		})
		if err != nil {
			return nil, nil, err
		}
		// TTY containers produce a raw (unframed) combined stream that must be
		// read verbatim; non-TTY containers produce an 8-byte-framed
		// multiplexed stream that must be demuxed with stdcopy.
		text, err := decodeJobLog(rc, st.Tty)
		closeErr := rc.Close()
		if err != nil {
			return nil, nil, err
		}
		if closeErr != nil {
			return nil, nil, closeErr
		}
		all += text
		perContainer[c.Name] = text
	}
	done, status, reason := fatalSetStatus(states, roles)

	labels := map[string]string{"status": status}
	if reason != "" {
		labels["reason"] = reason
	}

	// delete_on_success cleans up ONLY if the job actually finished (done: every
	// fatal-set container reached a terminal state) AND every fatal container
	// exited 0, matching the podman driver's `deleteOnSuccess && succeeded`.
	// Gating on done is a safety net: a mid-run get_job_log must never stop and
	// remove a still-running main container.
	if deleteOnSuccess && done && status == dipper.SUCCESS {
		if err := removeJob(ctx, api, job); err != nil {
			labels["cleanup_error"] = err.Error()
		}
	}

	return map[string]any{
		"all":        all,
		"containers": perContainer,
	}, labels, nil
}

func (d *dockerDriver) deleteJob(msg *dipper.Message) {
	log := d.GetLogger()
	log.Debugf("[%s] delete job with payload %+v", d.Service, msg.Payload)
	msg = dipper.DeserializePayload(msg)
	ctx, cancel := d.GetContext(msg)
	defer cancel()
	ctx, api := d.getJobAPI(ctx, msg)

	job := jobIDFromPayload(msg)
	if err := removeJob(ctx, api, job); err != nil {
		panic(err)
	}
	msg.Reply <- dipper.Message{Labels: map[string]string{"status": dipper.SUCCESS}}
}

// removeJob unconditionally cleans up a job: it removes ALL job containers in
// ANY state (running, stopped, created) BEFORE removing the driver-created work
// volume — Docker treats a volume as "in use" when any container in any state
// references it, so the volume removal fails otherwise. A pre-existing
// `use_work_volume` named volume is never touched. It is idempotent: a missing
// job/container is tolerated as a no-op (for re-queue/restart safety).
func removeJob(ctx context.Context, api jobAPI, job string) error {
	containers, err := api.ListContainers(ctx, job)
	if err != nil {
		if isNotFound(err) {
			return nil
		}

		return err
	}

	ownedVolume := ""
	for _, c := range containers {
		if c.WorkVolumeOwned && c.WorkVolume != "" {
			ownedVolume = c.WorkVolume
		}
		if err := removeOneContainer(ctx, api, c.ID); err != nil {
			if isNotFound(err) {
				continue
			}

			return err
		}
	}
	if ownedVolume != "" {
		if err := api.RemoveVolume(ctx, ownedVolume, false); err != nil {
			if isNotFound(err) {
				return nil
			}

			return err
		}
	}

	return nil
}

// removeOneContainer removes a single container, stopping it first if it is
// still running, then force-removing it.
func removeOneContainer(ctx context.Context, api jobAPI, id string) error {
	state, err := api.InspectContainer(ctx, id)
	if err != nil {
		return err
	}
	if state.Running {
		if err := api.StopContainer(ctx, id); err != nil {
			if isNotFound(err) {
				return nil
			}

			return err
		}
	}

	return api.RemoveContainer(ctx, id, true)
}

// decodeJobLog decodes a Docker Engine log stream into a single text blob.
// Docker logs are 8-byte-framed (stdout/stderr multiplexed) when the container
// was created WITHOUT a TTY, so the stream must be demultiplexed with stdcopy;
// otherwise every line carries a garbage header. A TTY container instead
// produces a raw, already-combined stream — running it through stdcopy would
// misinterpret the payload bytes as frame headers and corrupt the output — so
// TTY streams are read verbatim.
func decodeJobLog(r io.Reader, tty bool) (string, error) {
	if tty {
		b, err := io.ReadAll(r)

		return string(b), err
	}
	var buf bytes.Buffer
	if _, err := stdcopy.StdCopy(&buf, &buf, r); err != nil {
		return "", err
	}

	return buf.String(), nil
}

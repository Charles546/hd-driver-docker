// Copyright 2026 Chun Huang (Charles).

// This Source Code Form is dual-licensed.
// By default, this file is licensed under the GNU Affero General Public License v3.0.
// If you have a separate written commercial agreement, you may use this file under those terms instead.

package main

import (
	"bytes"
	"fmt"
	"time"

	"github.com/docker/docker/api/types/container"
	"github.com/docker/docker/pkg/stdcopy"
	"github.com/honeydipper/honeydipper/v4/pkg/dipper"
)

// exec runs a command inside a running job container via the Docker exec API,
// mirroring the podman driver's exec command. It supports `command` (array) or
// `script` + `shell` (default `sh -c`), demuxes the multiplexed 8-byte-framed
// exec stream with stdcopy (Tty:false), polls exec inspect for the exit code,
// and replies {output, exit_code} plus status/reason labels.
func (d *dockerDriver) exec(msg *dipper.Message) {
	log := d.GetLogger()
	log.Debugf("[%s] exec with payload %+v", d.Service, msg.Payload)
	msg = dipper.DeserializePayload(msg)
	ctx, cancel := d.GetContext(msg)
	defer cancel()
	ctx, api := d.getJobAPI(ctx, msg)

	payload, _ := msg.Payload.(map[string]any)
	if payload == nil {
		payload = map[string]any{}
	}

	containerID, _ := dipper.GetMapDataStr(payload, "container_id")
	if containerID == "" {
		msg.Reply <- dipper.Message{Labels: map[string]string{"status": dipper.ERROR, "reason": "missing container_id"}}

		return
	}

	cmdArgs, err := parseExecCommand(payload)
	if err != nil {
		msg.Reply <- dipper.Message{Labels: map[string]string{"status": dipper.ERROR, "reason": err.Error()}}

		return
	}

	// Create the exec session with Tty:false so the attached stream is the
	// multiplexed 8-byte-framed protocol that stdcopy can demux.
	execID, err := api.ExecCreate(ctx, containerID, container.ExecOptions{
		Cmd:          cmdArgs,
		AttachStdout: true,
		AttachStderr: true,
		Tty:          false,
	})
	if err != nil {
		panic(err)
	}

	stream, err := api.ExecAttach(ctx, execID, container.ExecAttachOptions{})
	if err != nil {
		panic(err)
	}
	defer func() { _ = stream.Close() }()

	// stdcopy.StdCopy blocks until the exec process exits (the stream EOF),
	// writing demuxed stdout+stderr into the same buffer, exactly like the
	// podman driver's ExecStartAndAttach.
	var buf bytes.Buffer
	if _, err := stdcopy.StdCopy(&buf, &buf, stream); err != nil {
		panic(err)
	}

	// Poll exec inspect for the exit code. After the attach stream EOF the exec
	// may briefly still report Running=true, so we wait for it to settle, then
	// read the exit code; ctx.Done() aborts the wait on command timeout.
	var inspect container.ExecInspect
	for {
		inspect, err = api.ExecInspect(ctx, execID)
		if err != nil {
			panic(err)
		}
		if !inspect.Running {
			break
		}
		select {
		case <-ctx.Done():
			return
		case <-time.After(100 * time.Millisecond):
		}
	}
	exitCode := inspect.ExitCode
	output := buf.String()

	status := dipper.SUCCESS
	reason := ""
	if exitCode != 0 {
		status = dipper.FAILURE
		reason = fmt.Sprintf("exit code %d", exitCode)
	}

	log.Debugf("[%s] exec output: %s", d.Service, output)

	msg.Reply <- dipper.Message{
		Payload: map[string]any{
			"output":    output,
			"exit_code": exitCode,
		},
		Labels: map[string]string{"status": status, "reason": reason},
	}
}

// parseExecCommand turns the exec payload into the exec command vector. It
// supports `command` (array, run directly; a string command is run via the
// shell) or `script` + `shell` (default `sh -c`), mirroring the podman driver.
// The shell defaults to `sh` when the payload omits `shell`.
func parseExecCommand(payload map[string]any) ([]string, error) {
	shell, _ := dipper.GetMapDataStr(payload, "shell")
	if shell == "" {
		shell = "sh"
	}

	if c, ok := payload["command"]; ok && c != nil {
		switch t := c.(type) {
		case []any:
			cmd := make([]string, 0, len(t))
			for _, v := range t {
				cmd = append(cmd, fmt.Sprintf("%v", v))
			}

			return cmd, nil
		case []string:
			return t, nil
		case string:
			return []string{shell, "-c", t}, nil
		default:
			return []string{shell, "-c", fmt.Sprintf("%v", t)}, nil
		}
	}
	if s, ok := dipper.GetMapDataStr(payload, "script"); ok && s != "" {
		return []string{shell, "-c", s}, nil
	}

	return nil, fmt.Errorf("missing cmd or script")
}

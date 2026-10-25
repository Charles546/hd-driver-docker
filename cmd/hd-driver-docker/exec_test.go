// Copyright 2026 Chun Huang (Charles).

// This Source Code Form is dual-licensed.
// By default, this file is licensed under the GNU Affero General Public License v3.0.
// If you have a separate written commercial agreement, you may use this file under those terms instead.

package main

import (
	"bytes"
	"io"
	"testing"

	"github.com/docker/docker/api/types/container"
	"github.com/honeydipper/honeydipper/v4/pkg/dipper"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestParseExecCommandArray(t *testing.T) {
	cmd, err := parseExecCommand(map[string]any{"command": []any{"echo", "hi"}})
	require.NoError(t, err)
	assert.Equal(t, []string{"echo", "hi"}, cmd)
}

func TestParseExecCommandStringViaShell(t *testing.T) {
	cmd, err := parseExecCommand(map[string]any{"command": "echo hi"})
	require.NoError(t, err)
	assert.Equal(t, []string{"sh", "-c", "echo hi"}, cmd)
}

func TestParseExecCommandScriptAndShell(t *testing.T) {
	cmd, err := parseExecCommand(map[string]any{"script": "ls -la", "shell": "bash"})
	require.NoError(t, err)
	assert.Equal(t, []string{"bash", "-c", "ls -la"}, cmd)
}

func TestParseExecCommandScriptDefaultShell(t *testing.T) {
	cmd, err := parseExecCommand(map[string]any{"script": "ls -la"})
	require.NoError(t, err)
	assert.Equal(t, []string{"sh", "-c", "ls -la"}, cmd)
}

func TestParseExecCommandMissing(t *testing.T) {
	_, err := parseExecCommand(map[string]any{})
	assert.Error(t, err)
	assert.Contains(t, err.Error(), "missing cmd or script")
}

// execMessage runs the exec handler with a scripted fake and returns the reply.
func execMessage(t *testing.T, api *fakeJobAPI, payload map[string]any) dipper.Message {
	t.Helper()
	d := newTestDriver(api)
	reply := make(chan dipper.Message, 1)
	d.exec(&dipper.Message{
		Payload: payload,
		Reply:   reply,
	})

	return <-reply
}

func TestExecCommandSuccess(t *testing.T) {
	api := newFakeJobAPI()
	api.execStreams = map[string]io.ReadCloser{
		"exec-c1": io.NopCloser(bytes.NewReader(framedLogStream("hello from exec"))),
	}
	api.execInspects = map[string]container.ExecInspect{
		"exec-c1": {Running: false, ExitCode: 0},
	}

	got := execMessage(t, api, map[string]any{"container_id": "c1", "command": []any{"echo", "hello"}})
	payload := got.Payload.(map[string]any)
	assert.Equal(t, "hello from exec\n", payload["output"])
	assert.Equal(t, 0, payload["exit_code"])
	assert.Equal(t, dipper.SUCCESS, got.Labels["status"])
	assert.Equal(t, "", got.Labels["reason"])
	assert.True(t, api.hasOp("execCreate:c1"))
	assert.True(t, api.hasOp("execAttach:exec-c1"))
	assert.True(t, api.hasOp("execInspect:exec-c1"))
}

func TestExecCommandNonZeroExit(t *testing.T) {
	api := newFakeJobAPI()
	api.execStreams = map[string]io.ReadCloser{
		"exec-c1": io.NopCloser(bytes.NewReader(framedLogStream("boom"))),
	}
	api.execInspects = map[string]container.ExecInspect{
		"exec-c1": {Running: false, ExitCode: 7},
	}

	got := execMessage(t, api, map[string]any{"container_id": "c1", "script": "exit 7"})
	payload := got.Payload.(map[string]any)
	assert.Equal(t, "boom\n", payload["output"])
	assert.Equal(t, 7, payload["exit_code"])
	assert.Equal(t, dipper.FAILURE, got.Labels["status"])
	assert.Contains(t, got.Labels["reason"], "exit code 7")
}

func TestExecCommandMissingContainerID(t *testing.T) {
	got := execMessage(t, newFakeJobAPI(), map[string]any{"command": []any{"echo", "hi"}})
	assert.Equal(t, dipper.ERROR, got.Labels["status"])
	assert.Contains(t, got.Labels["reason"], "missing container_id")
}

func TestExecCommandMissingCmdOrScript(t *testing.T) {
	got := execMessage(t, newFakeJobAPI(), map[string]any{"container_id": "c1"})
	assert.Equal(t, dipper.ERROR, got.Labels["status"])
	assert.Contains(t, got.Labels["reason"], "missing cmd or script")
}

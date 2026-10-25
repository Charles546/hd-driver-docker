// Copyright 2026 Chun Huang (Charles).

// This Source Code Form is dual-licensed.
// By default, this file is licensed under the GNU Affero General Public License v3.0.
// If you have a separate written commercial agreement, you may use this file under those terms instead.

package main

import (
	"bytes"
	"context"
	"testing"

	"github.com/docker/docker/api/types/container"
	"github.com/docker/docker/pkg/stdcopy"
	"github.com/honeydipper/honeydipper/v4/pkg/dipper"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestClampLimit(t *testing.T) {
	if got := clampLimit(-1); got != defaultTailMaxLines {
		t.Fatalf("expected default limit %d, got %d", defaultTailMaxLines, got)
	}
	if got := clampLimit(hardTailMaxLines + 123); got != hardTailMaxLines {
		t.Fatalf("expected hard max %d, got %d", hardTailMaxLines, got)
	}
	if got := clampLimit(321); got != 321 {
		t.Fatalf("expected 321, got %d", got)
	}
}

func TestParseContainerCursor(t *testing.T) {
	payload := map[string]any{
		"cursor": map[string]any{
			"containers": map[string]any{
				"c1": map[string]any{"ts": "2026-04-06T12:00:00Z", "skip": 7},
				"c2": 5,
			},
		},
	}

	cursor := parseContainerCursor(payload)
	if cursor["c1"].TS != "2026-04-06T12:00:00Z" || cursor["c1"].Skip != 7 {
		t.Fatalf("unexpected c1 cursor: %+v", cursor["c1"])
	}
	// legacy integer cursor: interpreted as skip with no timestamp.
	if cursor["c2"].TS != "" || cursor["c2"].Skip != 5 {
		t.Fatalf("unexpected c2 cursor: %+v", cursor["c2"])
	}
}

func TestParseContainerCursorFlatAndNegativeSkip(t *testing.T) {
	// flat (non-nested) cursor map, and negative skip clamped to 0.
	payload := map[string]any{
		"cursor": map[string]any{
			"c1": map[string]any{"ts": "2026-04-06T12:00:00Z", "skip": -3},
		},
	}
	cursor := parseContainerCursor(payload)
	if cursor["c1"].Skip != 0 {
		t.Fatalf("expected negative skip clamped to 0, got %d", cursor["c1"].Skip)
	}

	// no cursor at all -> empty map
	cursor = parseContainerCursor(map[string]any{})
	if len(cursor) != 0 {
		t.Fatalf("expected empty cursor, got %+v", cursor)
	}
}

func TestParseIncludeContainers(t *testing.T) {
	payload := map[string]any{
		"include_containers": []any{" init0 ", "main", ""},
	}
	include := parseIncludeContainers(payload)
	if len(include) != 2 {
		t.Fatalf("expected 2 names, got %+v", include)
	}
	if !include["init0"] || !include["main"] {
		t.Fatalf("expected init0 and main to be included, got %+v", include)
	}

	// absent or nil -> empty set
	if len(parseIncludeContainers(map[string]any{})) != 0 {
		t.Fatal("expected empty include set")
	}
}

func TestExtractLineTimestamp(t *testing.T) {
	ts := extractLineTimestamp("2026-04-06T12:00:00.123456789Z hello")
	if ts == "" {
		t.Fatal("expected RFC3339Nano timestamp to be detected")
	}

	ts = extractLineTimestamp("not-a-ts hello")
	if ts != "" {
		t.Fatalf("expected empty timestamp for invalid line, got %q", ts)
	}
}

func TestFilterByCursor(t *testing.T) {
	lines := []string{
		"2026-04-06T12:00:00Z one",
		"2026-04-06T12:00:00Z two",
		"2026-04-06T12:00:01Z three",
	}

	state := containerCursor{TS: "2026-04-06T12:00:00Z", Skip: 1}
	out := filterByCursor(lines, state)
	if len(out) != 2 {
		t.Fatalf("expected 2 lines after filtering, got %d (%+v)", len(out), out)
	}
	if out[0] != "2026-04-06T12:00:00Z two" || out[1] != "2026-04-06T12:00:01Z three" {
		t.Fatalf("unexpected filtered lines: %+v", out)
	}
}

func TestUpdateCursorWithLine(t *testing.T) {
	state := containerCursor{}
	state = updateCursorWithLine(state, "2026-04-06T12:00:00Z first")
	if state.TS != "2026-04-06T12:00:00Z" || state.Skip != 1 {
		t.Fatalf("unexpected state after first line: %+v", state)
	}

	state = updateCursorWithLine(state, "2026-04-06T12:00:00Z second")
	if state.Skip != 2 {
		t.Fatalf("expected skip=2 for same timestamp, got %+v", state)
	}

	state = updateCursorWithLine(state, "2026-04-06T12:00:01Z third")
	if state.TS != "2026-04-06T12:00:01Z" || state.Skip != 1 {
		t.Fatalf("unexpected state after new timestamp: %+v", state)
	}
}

func TestCollectLinesFromChunks(t *testing.T) {
	ch := make(chan string, 3)
	ch <- "2026-04-06T12:00:00Z one\n2026-04-06T12:00:00Z two\n"
	ch <- "2026-04-06T12:00:01Z three"
	close(ch)

	lines := collectLinesFromChunks(ch)
	require.Len(t, lines, 3)
	assert.Equal(t, "2026-04-06T12:00:00Z one", lines[0])
	assert.Equal(t, "2026-04-06T12:00:00Z two", lines[1])
	assert.Equal(t, "2026-04-06T12:00:01Z three", lines[2])
}

// framedLogStream builds a stdcopy-framed (multiplexed) log stream with each
// line written as its own stdout frame, the encoding a non-TTY container's
// Docker logs produce. TTY streams are raw; see rawLogStream.
func framedLogStream(lines ...string) []byte {
	var buf bytes.Buffer
	w := stdcopy.NewStdWriter(&buf, stdcopy.Stdout)
	for _, l := range lines {
		_, _ = w.Write([]byte(l + "\n"))
	}

	return buf.Bytes()
}

func rawLogStream(lines ...string) []byte {
	var buf bytes.Buffer
	for _, l := range lines {
		buf.WriteString(l + "\n")
	}

	return buf.Bytes()
}

// logTailJob is a helper that wires a fake jobAPI into a driver for the
// get_pod_log_tail handler tests.
func logTailJob(t *testing.T, api *fakeJobAPI) (*dockerDriver, chan dipper.Message) {
	t.Helper()
	d := newTestDriver(api)
	reply := make(chan dipper.Message, 1)

	return d, reply
}

func TestFetchContainerLinesSinceAndTail(t *testing.T) {
	api := newFakeJobAPI()
	api.logStreams = map[string][]byte{
		"c1": framedLogStream(
			"2026-04-06T12:00:00Z one",
			"2026-04-06T12:00:00Z two",
			"2026-04-06T12:00:01Z three",
		),
	}

	// no cursor + tailLimit -> Tail option set, Timestamps on, demuxed lines.
	lines := fetchContainerLines(context.Background(), api, "c1", false, containerCursor{}, 2)
	require.Len(t, lines, 3)
	opts := api.logOpts["c1"]
	assert.True(t, opts.Timestamps)
	assert.Equal(t, "2", opts.Tail)
	assert.Equal(t, "", opts.Since)

	// cursor ts -> Since option set (RFC3339Nano boundary), no Tail.
	_ = fetchContainerLines(context.Background(), api, "c1", false, containerCursor{TS: "2026-04-06T12:00:01Z"}, 0)
	opts = api.logOpts["c1"]
	assert.Equal(t, "2026-04-06T12:00:01Z", opts.Since)
	assert.Equal(t, "", opts.Tail)
}

func TestFetchContainerLinesTTYRaw(t *testing.T) {
	// A TTY container's stream is raw (not 8-byte-framed); decodeJobLog reads it
	// verbatim so the lines survive.
	api := newFakeJobAPI()
	api.logStreams = map[string][]byte{
		"c1": rawLogStream("2026-04-06T12:00:00Z one", "2026-04-06T12:00:01Z two"),
	}
	lines := fetchContainerLines(context.Background(), api, "c1", true, containerCursor{}, 0)
	require.Len(t, lines, 2)
	assert.Equal(t, "2026-04-06T12:00:00Z one", lines[0])
	assert.Equal(t, "2026-04-06T12:00:01Z two", lines[1])
}

// TestGetTailContainersDoneFromFatalSet verifies the container-state builder
// derives done/status from the shared fatalSetStatus: a running FATAL container
// holds done=false, and the status stays success while it runs.
func TestGetTailContainersDoneFromFatalSet(t *testing.T) {
	api := newFakeJobAPI()
	api.listResult = []jobContainer{
		{ID: "init0", Name: "init0", Role: string(roleInit), Index: 0},
		{ID: "main", Name: "main", Role: string(roleMain), Index: 1},
	}
	api.states["init0"] = containerState{ID: "init0", Name: "init0", Index: 0, Status: container.StateExited, ExitCode: 0}
	api.states["main"] = containerState{ID: "main", Name: "main", Index: 1, Status: container.StateRunning, Running: true}

	_, done, status, _ := getTailContainers(context.Background(), api, "hd-sess-1", nil)
	assert.False(t, done, "a running fatal container must hold done=false")
	assert.Equal(t, dipper.SUCCESS, status)
}

// TestFatalSetStatusNonFatalDoesNotHoldDone verifies Gap A at the shared
// fatalSetStatus level: a running non-fatal container (future sidecar) must NOT
// hold done=false, and its non-zero exit must NOT flip the streamed status. This
// is the guarantee the log-tail container-state builder inherits.
func TestFatalSetStatusNonFatalDoesNotHoldDone(t *testing.T) {
	roles := roles{init: []int{0}, main: 1}
	containers := []containerState{
		{Name: "init0", Index: 0, Status: container.StateExited, ExitCode: 0},
		{Name: "main", Index: 1, Status: container.StateExited, ExitCode: 0},
		{Name: "sidecar", Index: 9, Status: container.StateRunning, Running: true, ExitCode: 1},
	}
	done, status, reason := fatalSetStatus(containers, roles)
	assert.True(t, done, "a running non-fatal container must not hold done=false")
	assert.Equal(t, dipper.SUCCESS, status, "a non-fatal non-zero exit must not flip status")
	assert.Empty(t, reason)
}

func TestGetTailContainersRunningFatalNotDone(t *testing.T) {
	api := newFakeJobAPI()
	api.listResult = []jobContainer{
		{ID: "init0", Name: "init0", Role: string(roleInit), Index: 0},
		{ID: "main", Name: "main", Role: string(roleMain), Index: 1},
	}
	api.states["init0"] = containerState{ID: "init0", Name: "init0", Index: 0, Status: container.StateExited, ExitCode: 0}
	api.states["main"] = containerState{ID: "main", Name: "main", Index: 1, Status: container.StateRunning, Running: true}

	_, done, status, _ := getTailContainers(context.Background(), api, "hd-sess-1", nil)
	assert.False(t, done, "a running fatal container must hold done=false")
	assert.Equal(t, dipper.SUCCESS, status)
}

func TestGetTailContainersIncludeFilterByName(t *testing.T) {
	api := newFakeJobAPI()
	api.listResult = []jobContainer{
		{ID: "init0", Name: "init0", Role: string(roleInit), Index: 0},
		{ID: "main", Name: "main", Role: string(roleMain), Index: 1},
	}
	api.states["init0"] = containerState{ID: "init0", Name: "init0", Index: 0, Status: container.StateExited, ExitCode: 0}
	api.states["main"] = containerState{ID: "main", Name: "main", Index: 1, Status: container.StateExited, ExitCode: 0}

	// include by the FULL suffixed container name (that is why Phase 1's
	// -hd-<sessionID>-<cursor>-c suffix scheme matters).
	containers, done, status, _ := getTailContainers(context.Background(), api, "hd-sess-1", map[string]bool{"main": true})
	require.Len(t, containers, 1)
	assert.Equal(t, "main", containers[0].Name)
	assert.True(t, done, "done still reflects the whole fatal set even when filtered")
	assert.Equal(t, dipper.SUCCESS, status)
}

func TestBuildTailChunk(t *testing.T) {
	api := newFakeJobAPI()
	api.listResult = []jobContainer{
		{ID: "init0", Name: "init0", Role: string(roleInit), Index: 0},
		{ID: "main", Name: "main", Role: string(roleMain), Index: 1},
	}
	api.states["init0"] = containerState{ID: "init0", Name: "init0", Index: 0, Status: container.StateExited, ExitCode: 0}
	api.states["main"] = containerState{ID: "main", Name: "main", Index: 1, Status: container.StateExited, ExitCode: 0}
	api.logStreams = map[string][]byte{
		"init0": framedLogStream("2026-04-06T12:00:00Z init one"),
		"main": framedLogStream(
			"2026-04-06T12:00:00Z main one",
			"2026-04-06T12:00:00Z main two",
			"2026-04-06T12:00:01Z main three",
		),
	}

	lines, nextCursor, hasMore, done, status, reason := buildTailChunk(context.Background(), api, "hd-sess-1", nil, nil, defaultTailMaxLines, defaultTailMaxLines)
	require.Len(t, lines, 4)
	assert.Equal(t, "init0", lines[0].Container)
	assert.Equal(t, "2026-04-06T12:00:00Z init one", lines[0].Line)
	assert.Equal(t, "main", lines[1].Container)
	assert.Equal(t, "2026-04-06T12:00:00Z main one", lines[1].Line)
	// cursor: init0 emitted 1 line at 12:00:00Z; main emitted 3 lines ending at
	// 12:00:01Z (skip resets to 1 on the new timestamp).
	assert.Equal(t, "2026-04-06T12:00:00Z", nextCursor["init0"].TS)
	assert.Equal(t, 1, nextCursor["init0"].Skip)
	assert.Equal(t, "2026-04-06T12:00:01Z", nextCursor["main"].TS)
	assert.Equal(t, 1, nextCursor["main"].Skip)
	assert.True(t, done)
	assert.Equal(t, dipper.SUCCESS, status)
	assert.False(t, hasMore)
	assert.Empty(t, reason)
}

func TestGetPodLogTailReplyShape(t *testing.T) {
	api := newFakeJobAPI()
	api.listResult = []jobContainer{
		{ID: "main", Name: "main", Role: string(roleMain), Index: 0},
	}
	api.states["main"] = containerState{ID: "main", Name: "main", Index: 0, Status: container.StateExited, ExitCode: 0}
	api.logStreams = map[string][]byte{
		"main": framedLogStream("2026-04-06T12:00:00Z hello"),
	}

	d, reply := logTailJob(t, api)
	d.getPodLogTail(&dipper.Message{
		Payload: map[string]any{"pod_id": "hd-sess-1", "max_lines": 200},
		Reply:   reply,
	})
	got := <-reply
	payload, ok := got.Payload.(map[string]any)
	require.True(t, ok)
	require.Len(t, payload["lines"], 1)
	lines := payload["lines"].([]tailLine)
	assert.Equal(t, "main", lines[0].Container)
	assert.Equal(t, "2026-04-06T12:00:00Z hello", lines[0].Line)

	nc, ok := payload["next_cursor"].(map[string]any)
	require.True(t, ok)
	containers, ok := nc["containers"].(map[string]containerCursor)
	require.True(t, ok)
	assert.Equal(t, "2026-04-06T12:00:00Z", containers["main"].TS)
	assert.Equal(t, 1, containers["main"].Skip)

	assert.False(t, payload["has_more"].(bool))
	assert.True(t, payload["done"].(bool))
	assert.False(t, payload["truncated"].(bool))
	assert.Equal(t, dipper.SUCCESS, got.Labels["status"])
}

func TestGetPodLogTailDoneMaxLinesTwoPhase(t *testing.T) {
	api := newFakeJobAPI()
	api.listResult = []jobContainer{
		{ID: "main", Name: "main", Role: string(roleMain), Index: 0},
	}
	api.states["main"] = containerState{ID: "main", Name: "main", Index: 0, Status: container.StateExited, ExitCode: 0}
	// 3 lines: with max_lines=2 the first chunk is truncated, then the done
	// phase re-chunks with done_max_lines=5000 and returns all 3.
	api.logStreams = map[string][]byte{
		"main": framedLogStream(
			"2026-04-06T12:00:00Z one",
			"2026-04-06T12:00:01Z two",
			"2026-04-06T12:00:02Z three",
		),
	}

	d, reply := logTailJob(t, api)
	d.getPodLogTail(&dipper.Message{
		Payload: map[string]any{"pod_id": "hd-sess-1", "max_lines": 2, "done_max_lines": 5000},
		Reply:   reply,
	})
	got := <-reply
	payload := got.Payload.(map[string]any)
	require.Len(t, payload["lines"], 3, "done phase must re-fetch with done_max_lines")
	assert.True(t, payload["done"].(bool))
	assert.False(t, payload["truncated"].(bool), "done phase with enough lines is not truncated")
}

func TestGetPodLogTailFailureStatus(t *testing.T) {
	api := newFakeJobAPI()
	api.listResult = []jobContainer{
		{ID: "main", Name: "main", Role: string(roleMain), Index: 0},
	}
	api.states["main"] = containerState{ID: "main", Name: "main", Index: 0, Status: container.StateExited, ExitCode: 3}
	api.logStreams = map[string][]byte{
		"main": framedLogStream("2026-04-06T12:00:00Z oops"),
	}

	d, reply := logTailJob(t, api)
	d.getPodLogTail(&dipper.Message{
		Payload: map[string]any{"pod_id": "hd-sess-1"},
		Reply:   reply,
	})
	got := <-reply
	assert.Equal(t, dipper.FAILURE, got.Labels["status"])
	assert.Contains(t, got.Labels["reason"], "main")
	assert.Contains(t, got.Labels["reason"], "exit code 3")
}

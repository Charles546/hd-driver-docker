// Copyright 2026 Chun Huang (Charles).

// This Source Code Form is dual-licensed.
// By default, this file is licensed under the GNU Affero General Public License v3.0.
// If you have a separate written commercial agreement, you may use this file under those terms instead.

package main

import (
	"context"
	"fmt"
	"strconv"
	"strings"
	"time"

	"github.com/docker/docker/api/types/container"
	"github.com/honeydipper/honeydipper/v4/pkg/dipper"
)

const (
	defaultTailWaitSeconds = 3
	defaultTailMaxLines    = 200
	defaultDoneMaxLines    = 5000
	hardTailMaxLines       = 10000
	tailPollInterval       = 300 * time.Millisecond
)

// tailLine is one log line emitted in a get_pod_log_tail reply chunk.
type tailLine struct {
	Container string `json:"container"`
	Line      string `json:"line"`
	Index     int    `json:"index"`
}

// containerCursor is the per-container streaming cursor. TS is the timestamp of
// the last emitted line and Skip is how many lines at that boundary timestamp
// were already emitted. Because Docker's `since` filter is inclusive at the
// boundary timestamp, Skip dedupes the boundary lines on the next fetch.
type containerCursor struct {
	TS   string `json:"ts"`
	Skip int    `json:"skip"`
}

// toInt converts a payload value to an int, falling back when unparseable. It is
// used for both max_lines-style numeric fields and the legacy integer cursors.
func toInt(v any, fallback int) int {
	switch t := v.(type) {
	case int:
		return t
	case int8:
		return int(t)
	case int16:
		return int(t)
	case int32:
		return int(t)
	case int64:
		return int(t)
	case uint:
		return int(t)
	case uint8:
		return int(t)
	case uint16:
		return int(t)
	case uint32:
		return int(t)
	case uint64:
		return int(t)
	case float32:
		return int(t)
	case float64:
		return int(t)
	case string:
		i, err := strconv.Atoi(t)
		if err == nil {
			return i
		}
	}

	return fallback
}

// parseContainerCursor parses the `cursor` payload field. The cursor may be a
// flat map of container name -> {ts, skip}, or nested under a `containers` key.
// A bare integer value is accepted for backward compatibility as a legacy
// integer cursor (interpreted as skip with no timestamp). skip is clamped >= 0.
func parseContainerCursor(payload map[string]any) map[string]containerCursor {
	ret := map[string]containerCursor{}
	raw, ok := payload["cursor"]
	if !ok || raw == nil {
		return ret
	}

	cursorMap, ok := raw.(map[string]any)
	if !ok {
		return ret
	}

	if nested, found := cursorMap["containers"]; found && nested != nil {
		if nestedMap, isMap := nested.(map[string]any); isMap {
			cursorMap = nestedMap
		}
	}

	for name, item := range cursorMap {
		state := containerCursor{}
		switch t := item.(type) {
		case map[string]any:
			if ts, ok := t["ts"].(string); ok {
				state.TS = strings.TrimSpace(ts)
			}
			state.Skip = toInt(t["skip"], 0)
		default:
			// Backward compatibility for legacy index cursors.
			state.Skip = toInt(t, 0)
		}
		if state.Skip < 0 {
			state.Skip = 0
		}
		ret[name] = state
	}

	return ret
}

// parseIncludeContainers parses the `include_containers` payload field into a
// set of container names. The set is matched against the job's container NAMES
// (the full `-hd-<sessionID>-<cursor>-c` suffixed names), which is why Phase 1
// names containers deterministically.
func parseIncludeContainers(payload map[string]any) map[string]bool {
	ret := map[string]bool{}
	raw, ok := payload["include_containers"]
	if !ok || raw == nil {
		return ret
	}

	switch t := raw.(type) {
	case []any:
		for _, v := range t {
			name := strings.TrimSpace(interfaceToString(v))
			if name != "" {
				ret[name] = true
			}
		}
	case []string:
		for _, v := range t {
			name := strings.TrimSpace(v)
			if name != "" {
				ret[name] = true
			}
		}
	}

	return ret
}

// interfaceToString renders a payload value as a string.
func interfaceToString(v any) string {
	return fmt.Sprintf("%v", v)
}

// collectLinesFromChunks splits a stream of text chunks into complete lines. A
// partial final line without a trailing newline is still emitted. Empty lines
// are dropped to keep the cursor index math aligned with non-empty log lines.
func collectLinesFromChunks(ch <-chan string) []string {
	lines := []string{}
	carry := ""

	for chunk := range ch {
		if chunk == "" {
			continue
		}

		carry += chunk
		for {
			idx := strings.IndexByte(carry, '\n')
			if idx < 0 {
				break
			}

			line := carry[:idx]
			if line != "" {
				lines = append(lines, line)
			}
			carry = carry[idx+1:]
		}
	}

	if carry != "" {
		lines = append(lines, carry)
	}

	return lines
}

// extractLineTimestamp pulls the RFC3339(Nano) timestamp prefix off a log line
// (the format Docker emits when LogsOptions.Timestamps is set). Returns "" when
// the line has no parseable timestamp.
func extractLineTimestamp(line string) string {
	line = strings.TrimSpace(line)
	if line == "" {
		return ""
	}

	space := strings.IndexByte(line, ' ')
	if space <= 0 {
		return ""
	}

	ts := line[:space]
	if _, err := time.Parse(time.RFC3339Nano, ts); err == nil {
		return ts
	}
	if _, err := time.Parse(time.RFC3339, ts); err == nil {
		return ts
	}

	return ""
}

// getTailContainers builds the container-state list for the log-tail RPC,
// optionally filtered by container NAME via include_containers. The done/status/
// reason triplet comes from the shared fatalSetStatus over the FATAL set only
// (Gap A): a long-running non-fatal container must NOT hold done=false, and a
// non-fatal container's non-zero exit must NOT flip the streamed status.
func getTailContainers(ctx context.Context, api jobAPI, job string, include map[string]bool) ([]containerState, bool, string, string) {
	containers, err := findJobContainers(ctx, api, job)
	if err != nil {
		panic(err)
	}
	roles := rolesOf(containers)
	states, err := inspectJobContainers(ctx, api, containers)
	if err != nil {
		panic(err)
	}
	done, status, reason := fatalSetStatus(states, roles)

	ret := []containerState{}
	for _, st := range states {
		if len(include) > 0 && !include[st.Name] {
			continue
		}
		ret = append(ret, st)
	}

	return ret, done, status, reason
}

// fetchContainerLines fetches one container's log lines via the Docker logs API.
// It requests Timestamps:true; when a cursor timestamp is present it is used as
// the (inclusive) `since` boundary, otherwise a `tail` limit is used for the
// first fetch. The stream is demuxed with stdcopy (TTY streams read verbatim),
// reusing the Phase 1 decodeJobLog helper.
func fetchContainerLines(ctx context.Context, api jobAPI, containerID string, tty bool, state containerCursor, tailLimit int) []string {
	opts := container.LogsOptions{
		ShowStdout: true,
		ShowStderr: true,
		Timestamps: true,
	}
	if state.TS != "" {
		opts.Since = state.TS
	} else if tailLimit > 0 {
		opts.Tail = strconv.Itoa(tailLimit)
	}

	rc, err := api.ContainerLogs(ctx, containerID, opts)
	if err != nil {
		panic(err)
	}
	defer func() { _ = rc.Close() }()

	text, err := decodeJobLog(rc, tty)
	if err != nil {
		panic(err)
	}

	return collectLinesFromChunks(singleChunk(text))
}

// singleChunk wraps a text blob in a one-element channel for collectLinesFromChunks.
func singleChunk(text string) <-chan string {
	ch := make(chan string, 1)
	ch <- text
	close(ch)

	return ch
}

// clampLimit clamps a requested max_lines to [defaultTailMaxLines, hardTailMaxLines].
func clampLimit(limit int) int {
	if limit <= 0 {
		limit = defaultTailMaxLines
	}
	if limit > hardTailMaxLines {
		limit = hardTailMaxLines
	}

	return limit
}

// updateCursorWithLine advances a container cursor past one emitted line. Lines
// sharing the cursor's boundary timestamp increment Skip (they are the lines
// Docker's inclusive `since` would re-serve); a new timestamp resets Skip to 1.
func updateCursorWithLine(state containerCursor, line string) containerCursor {
	ts := extractLineTimestamp(line)
	if ts == "" {
		state.Skip++

		return state
	}

	if state.TS == ts {
		state.Skip++

		return state
	}

	state.TS = ts
	state.Skip = 1

	return state
}

// filterByCursor drops lines the cursor has already emitted: all lines before
// the cursor timestamp (Docker's inclusive `since` re-serves them) plus the
// first Skip lines at the boundary timestamp.
func filterByCursor(lines []string, state containerCursor) []string {
	if state.TS == "" || state.Skip <= 0 {
		return lines
	}

	ret := []string{}
	boundarySeen := 0
	for _, line := range lines {
		ts := extractLineTimestamp(line)
		if ts == state.TS && boundarySeen < state.Skip {
			boundarySeen++
			continue
		}
		ret = append(ret, line)
	}

	return ret
}

// buildTailChunk assembles one get_pod_log_tail reply chunk: it fetches each
// included container's lines, applies the cursor filter, emits up to `limit`
// lines, and computes the next cursor plus has_more/done/status/reason. done and
// status come from the shared fatalSetStatus over the fatal set only.
func buildTailChunk(ctx context.Context, api jobAPI, job string, cursor map[string]containerCursor, include map[string]bool, limit int, tailForEmptyCursor int) ([]tailLine, map[string]containerCursor, bool, bool, string, string) {
	containers, done, status, reason := getTailContainers(ctx, api, job, include)

	lines := []tailLine{}
	nextCursor := map[string]containerCursor{}
	hasMore := false

	for _, c := range containers {
		state := cursor[c.Name]
		rawLines := fetchContainerLines(ctx, api, c.ID, c.Tty, state, tailForEmptyCursor)
		candidateLines := filterByCursor(rawLines, state)

		remaining := limit - len(lines)
		emitCount := len(candidateLines)
		if remaining < emitCount {
			emitCount = remaining
		}

		for i := 0; i < emitCount; i++ {
			line := candidateLines[i]
			lines = append(lines, tailLine{
				Container: c.Name,
				Line:      line,
				Index:     state.Skip,
			})
			state = updateCursorWithLine(state, line)
		}

		nextCursor[c.Name] = state
		if emitCount < len(candidateLines) {
			hasMore = true
		}

		if len(lines) >= limit {
			for _, cc := range containers {
				if _, ok := nextCursor[cc.Name]; ok {
					continue
				}
				nextCursor[cc.Name] = cursor[cc.Name]
			}
			hasMore = true

			break
		}
	}

	return lines, nextCursor, hasMore, done, status, reason
}

// getPodLogTail is the RPC handler that streams a job's container logs to the
// operator/UI in chunks, mirroring the podman driver's reply shape:
//
//	{ lines, next_cursor: { containers: {...} }, has_more, done, truncated }
//
// plus status/reason labels. It polls every ~300ms until lines arrive, the job
// is done, or the wait_seconds deadline passes.
//
// Done-stream note: when done=true the final fetch is widened to done_max_lines
// (the two-phase re-chunk). If a non-fatal container is still running at that
// point, its post-main-exit lines simply end the stream here — they remain
// retrievable via get_job_log — because the stream is considered complete once
// the fatal set has reached its terminal state.
func (d *dockerDriver) getPodLogTail(msg *dipper.Message) {
	log := d.GetLogger()
	log.Debugf("[%s] get pod log tail with payload %+v", d.Service, msg.Payload)
	msg = dipper.DeserializePayload(msg)
	ctx, cancel := d.GetContext(msg)
	defer cancel()
	ctx, api := d.getJobAPI(ctx, msg)

	payload, _ := msg.Payload.(map[string]any)
	if payload == nil {
		payload = map[string]any{}
	}

	job := jobIDFromPayload(msg)

	waitSeconds := toInt(payload["wait_seconds"], defaultTailWaitSeconds)
	if waitSeconds < 0 {
		waitSeconds = 0
	}
	maxLines := clampLimit(toInt(payload["max_lines"], defaultTailMaxLines))
	doneMaxLines := clampLimit(toInt(payload["done_max_lines"], defaultDoneMaxLines))
	cursor := parseContainerCursor(payload)
	include := parseIncludeContainers(payload)

	deadline := time.Now().Add(time.Duration(waitSeconds) * time.Second)
	for {
		lines, nextCursor, hasMore, done, status, reason := buildTailChunk(ctx, api, job, cursor, include, maxLines, maxLines)
		limitUsed := maxLines
		if done {
			limitUsed = doneMaxLines
			if limitUsed != maxLines {
				lines, nextCursor, hasMore, done, status, reason = buildTailChunk(ctx, api, job, cursor, include, limitUsed, doneMaxLines)
			}
		}

		if len(lines) > 0 || done || waitSeconds == 0 || time.Now().After(deadline) {
			msg.Reply <- dipper.Message{
				Payload: map[string]any{
					"lines":       lines,
					"next_cursor": map[string]any{"containers": nextCursor},
					"has_more":    hasMore,
					"done":        done,
					"truncated":   hasMore && len(lines) >= limitUsed,
				},
				Labels: map[string]string{
					"status": status,
					"reason": reason,
				},
			}

			return
		}

		time.Sleep(tailPollInterval)
	}
}

# Honeydipper Driver for Docker

`hd-driver-docker` lets [Honeydipper](https://github.com/honeydipper/honeydipper)
run run-to-complete jobs with **Docker Engine** on a laptop — no Kubernetes
cluster required. It is modeled on
[`Charles546/hd-driver-podman`](https://github.com/Charles546/hd-driver-podman),
which runs the same kind of jobs with podman, but it drives the Docker Engine
API (`github.com/docker/docker`) instead of podman bindings.

## Why a driver at all?

Docker has no pod/Job object, so a "job" is a **named group of containers**:

- **init containers** — all but the last entry in the job's `containers` list.
  They run sequentially, one at a time, and each must exit `0` before the next
  one starts.
- **main container** — the last entry. It runs only after every init container
  succeeds.
- All containers share a **work volume** mounted at `work_volume_mount_point`
  (default `/local/honeydipper`).

Because Docker has no native init-container sequencing, the driver implements
the sequencing state machine itself in `start_job`. The two shared pure
functions — `resolveRoles` and `fatalSetStatus` — are the forward-compatibility
hooks: a future sidecar extension (concurrent, non-fatal containers) can be
added by teaching `resolveRoles` to read an optional `mode` field, with no
structural rework.

## Phase 1 scope

Phase 1 implements the driver core — the run-to-complete job lifecycle:

| Command | Interruptible | Purpose |
|---|---|---|
| `create_job` | | Pull images, create (but do not start) all containers, set up the shared work volume. Replies `{pod_id, container_ids}`. |
| `start_job` | yes (30m) | Docker-native init sequencing: run init containers one-by-one, fail fast on an init failure (skipping main), then start main. Resumable on re-queue. |
| `wait_job` | yes (30m) | Blocks until the **main** container reaches a terminal state, then reports success/failure over the fatal set. |
| `get_job_log` | | Combined stdout/stderr for all containers (with timestamps), plus `status`/`reason`; honors `delete_on_success`. |
| `delete_job` | | Unconditional cleanup of the whole job (containers in any state, then the driver-created work volume). Idempotent. |

`exec`, `create_volume`, `delete_volume`, and the `get_pod_log_tail` RPC are
registered in `main.go` as part of the stable command surface but belong to
**Phase 2** (exec, volumes, log-tail streaming RPC); until then they reply with
a clean "not implemented in phase 1" error.

## Message shape

`create_job` accepts a `containers` list plus optional `pod_spec`-style keys:

```yaml
containers:
  - image: busybox
    name: prepare
    command: ["sh", "-c", "echo preparing > /local/honeydipper/ready"]
    imagePullPolicy: IfNotPresent
  - image: busybox
    name: main
    command: ["sh", "-c", "cat /local/honeydipper/ready"]
with_work_volume: true            # create an ephemeral shared volume
# use_work_volume: my-persistent  # OR mount a pre-existing named volume
work_volume_mount_point: /local/honeydipper
```

`create_job` replies with `pod_id` (the job identifier, e.g.
`hd-<sessionID>-<cursor>`) and `container_ids` (a map of container name → id).
`start_job` / `wait_job` / `get_job_log` / `delete_job` take `pod_id` and look
up the job's containers by the `hd-job=<pod_id>` label, so they are
**restart-safe**: no in-memory state is required to resume an interrupted or
re-queued command.

## Cleanup policy

- The **daemon never cleans up** after a job. Jobs **linger by default** after
  the workflow finishes, so logs and artifacts stay inspectable.
- **Success cleanup** is opt-in via `delete_on_success: true` on `get_job_log`,
  which deletes the job **only** if the main container and all init containers
  exited `0` (matching podman's `deleteOnSuccess && succeeded`).
- **Failure cleanup** is explicit: call `delete_job` (or set `delete_on_success`
  and rely on `wait_job`'s failure path).
- The **driver-created work volume dies with the job**: `delete_job` removes all
  job containers in any state (running → stop-then-remove, otherwise
  force-remove) **before** removing the work volume, because Docker treats a
  volume as "in use" if any container in any state references it.
- **Pre-existing volumes are never touched**: a `use_work_volume` named volume
  is mounted but is not owned by the driver and is never removed.
- `delete_job` is idempotent — a missing job/container is a no-op (for
  re-queue/restart safety).

## Connection handling

The driver builds its Docker client from the standard Docker environment
(`DOCKER_HOST`, `DOCKER_TLS_VERIFY`, `DOCKER_CERT_PATH`) via `client.FromEnv()`,
with an optional per-message `uri` override and `WithAPIVersionNegotiation()` so
the SDK negotiates a compatible API version with older laptop Docker daemons.

A malformed `DOCKER_TLS_VERIFY` / `DOCKER_CERT_PATH` is routed through the
driver's error-handling/recover path (the same `dipper.Must` pattern the podman
driver's `getConnection` uses), so it returns a clean error reply instead of
killing the process.

> **`identity` is ignored.** The payload key `identity` is accepted for
> message-shape compatibility with the config bundle, but it is deliberately
> not applied: unlike podman's SSH identity there is no TLS client-identity
> analogue when talking to a Docker daemon.

## Deliberate module-path deviation

This repo is a **standalone Go module**: the module path is
`github.com/Charles546/hd-driver-docker`, identical to the repository path.

This is a deliberate improvement over `hd-driver-podman`, which lives in the
shared module `github.com/Charles546/honeydipper-drivers` (its module path does
not match its repo path). A module whose path diverges from its repo path only
works because nothing imports it by module path — a fragile arrangement.
Likewise, the OpenAI driver repo is `Charles546/hd-driver-openai`, whose module
path also diverges from its repo path and only works because nothing imports
it. Our choice — module path = repo path — means the module is importable
correctly by path and is the pattern to follow for future drivers.

## License

This project is prepared for dual licensing (mirroring `hd-driver-podman`):

- `LICENSE` — GNU Affero General Public License v3.0
- `LICENSE-COMMERCIAL.md` — commercial licensing path for organizations that
  want to use the software outside the AGPL terms

The AGPL license applies by default unless you have a separate written
commercial agreement with the copyright holder.

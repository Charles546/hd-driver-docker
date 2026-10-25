// Copyright 2026 Chun Huang (Charles).

// This Source Code Form is dual-licensed.
// By default, this file is licensed under the GNU Affero General Public License v3.0.
// If you have a separate written commercial agreement, you may use this file under those terms instead.

// Package main implements the Honeydipper Docker driver. It lets Honeydipper run
// run-to-complete jobs with Docker Engine on a laptop, without a Kubernetes
// cluster. Because Docker has no pod/Job object, a "job" is a named group of
// containers: all but the last run sequentially as init containers, the last is
// the main container, and all share a work volume.
package main

import (
	"context"
	"flag"
	"fmt"
	"os"

	"github.com/docker/docker/client"
	"github.com/honeydipper/honeydipper/v4/pkg/dipper"
)

type dockerDriver struct {
	*dipper.Driver
}

var docker = &dockerDriver{}

func initFlags() {
	flag.Usage = func() {
		fmt.Printf("%s [ -h ] <service name>\n", os.Args[0])
		fmt.Printf("    This driver supports the operator service only.\n")
		fmt.Printf("  This program provides honeydipper with the capability of running jobs with containers using Docker Engine.\n")
	}
}

func main() {
	initFlags()
	flag.Parse()
	docker.Driver = dipper.NewDriver(flag.Arg(0), "docker")
	docker.Commands["create_job"] = docker.createJob
	docker.Commands["start_job|interruptible"] = docker.startJob
	docker.DefaultTimeout["start_job"] = "30m"
	docker.Commands["wait_job|interruptible"] = docker.waitJob
	docker.DefaultTimeout["wait_job"] = "30m"
	docker.Commands["get_job_log"] = docker.getJobLog
	docker.Commands["delete_job"] = docker.deleteJob
	// exec / create_volume / delete_volume / get_pod_log_tail belong to Phase 2
	// (exec, volumes, log-tail streaming RPC). They are registered now so the
	// driver's command surface is stable; until Phase 2 lands their handlers
	// reply with a clean "not implemented in phase 1" error.
	docker.Commands["exec"] = docker.exec
	docker.Commands["create_volume"] = docker.createVolume
	docker.Commands["delete_volume"] = docker.deleteVolume
	docker.RPCHandlers["get_pod_log_tail"] = docker.getPodLogTail
	docker.Reload = func(m *dipper.Message) {}
	docker.Run()
}

// getConnection builds a Docker Engine API client for the message. It honors the
// standard Docker environment (DOCKER_HOST / DOCKER_TLS_VERIFY / DOCKER_CERT_PATH)
// via client.FromEnv, with an optional per-message `uri` override. Any error from
// a malformed DOCKER_TLS_VERIFY / DOCKER_CERT_PATH is routed through dipper.Must
// (the same pattern the podman driver's getConnection uses), which the command
// wrapper turns into a clean error reply instead of killing the process.
func (d *dockerDriver) getConnection(ctx context.Context, m *dipper.Message) (*client.Client, context.Context) {
	opts, err := dockerClientOpts(m)
	if err != nil {
		panic(err)
	}
	cli := dipper.Must(client.NewClientWithOpts(opts...)).(*client.Client)

	return cli, ctx
}

// dockerClientOpts assembles the Docker client options for a message. It routes
// any panic from client.FromEnv (e.g. a malformed DOCKER_TLS_VERIFY /
// DOCKER_CERT_PATH) into a clean error so it can be surfaced through the driver's
// recover path rather than crashing the process.
func dockerClientOpts(m *dipper.Message) (opts []client.Opt, err error) {
	defer func() {
		if r := recover(); r != nil {
			err = fmt.Errorf("invalid docker client configuration: %v", r)
		}
	}()

	opts = []client.Opt{client.FromEnv}
	if uri, ok := dipper.GetMapDataStr(m.Payload, "uri"); ok {
		opts = append(opts, client.WithHost(uri))
	}
	// `identity` is accepted for message-shape compatibility with the config
	// bundle but deliberately ignored: there is no TLS client-identity analogue
	// to apply when talking to a Docker daemon.
	opts = append(opts, client.WithAPIVersionNegotiation())

	return opts, nil
}

// getJobAPI wraps the per-message connection in the jobAPI abstraction used by
// the job lifecycle so the lifecycle logic is unit-testable.
func (d *dockerDriver) getJobAPI(ctx context.Context, m *dipper.Message) (context.Context, jobAPI) {
	conn, ctx := d.getConnection(ctx, m)

	return ctx, newDockerJobAPI(conn)
}

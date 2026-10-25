// Copyright 2026 Chun Huang (Charles).

// This Source Code Form is dual-licensed.
// By default, this file is licensed under the GNU Affero General Public License v3.0.
// If you have a separate written commercial agreement, you may use this file under those terms instead.

package main

import (
	"fmt"

	"github.com/honeydipper/honeydipper/v4/pkg/dipper"
)

// The exec / create_volume / delete_volume commands and the get_pod_log_tail RPC
// are registered in main.go as part of the driver's stable command surface, but
// they belong to Phase 2 (exec, volumes, log-tail streaming RPC). Until Phase 2
// lands they reply with a clean "not implemented in phase 1" error.

func (d *dockerDriver) exec(msg *dipper.Message) {
	notImplemented(msg, "exec")
}

func (d *dockerDriver) createVolume(msg *dipper.Message) {
	notImplemented(msg, "create_volume")
}

func (d *dockerDriver) deleteVolume(msg *dipper.Message) {
	notImplemented(msg, "delete_volume")
}

func (d *dockerDriver) getPodLogTail(msg *dipper.Message) {
	notImplemented(msg, "get_pod_log_tail")
}

// notImplemented replies with a clean "not implemented in phase 1" error.
func notImplemented(msg *dipper.Message, method string) {
	msg = dipper.DeserializePayload(msg)
	msg.Reply <- dipper.Message{Labels: map[string]string{
		"status": dipper.ERROR,
		"reason": fmt.Sprintf("%s is not implemented in phase 1", method),
	}}
}

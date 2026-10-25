// Copyright 2026 Chun Huang (Charles).

// This Source Code Form is dual-licensed.
// By default, this file is licensed under the GNU Affero General Public License v3.0.
// If you have a separate written commercial agreement, you may use this file under those terms instead.

package main

import (
	"testing"

	"github.com/honeydipper/honeydipper/v4/pkg/dipper"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// runVolume invokes a volume command handler with a scripted fake and returns
// the reply message.
func runVolume(t *testing.T, d *dockerDriver, handler func(*dipper.Message), payload map[string]any) dipper.Message {
	t.Helper()
	reply := make(chan dipper.Message, 1)
	handler(&dipper.Message{
		Payload: payload,
		Reply:   reply,
	})

	return <-reply
}

// newVolumeDriver builds a driver whose jobAPI is a fake, for handler-level
// volume command tests.
func newVolumeDriver(api jobAPI) *dockerDriver {
	return newTestDriver(api)
}

func TestCreateVolumeSimple(t *testing.T) {
	api := newFakeJobAPI()
	d := newVolumeDriver(api)
	got := runVolume(t, d, d.createVolume, map[string]any{"name": "my-vol"})
	assert.Equal(t, "my-vol", got.Payload.(map[string]any)["volume_name"])
	assert.True(t, api.hasOp("createVolume:my-vol"))
}

func TestCreateVolumeWithSpec(t *testing.T) {
	api := newFakeJobAPI()
	d := newVolumeDriver(api)
	got := runVolume(t, d, d.createVolume, map[string]any{
		"name": "my-vol",
		"volume_spec": map[string]any{
			"driver":      "local",
			"driver_opts": map[string]any{"type": "tmpfs"},
			"labels":      map[string]any{"team": "platform"},
		},
	})
	assert.Equal(t, "my-vol", got.Payload.(map[string]any)["volume_name"])
	require.Len(t, api.createdVolumes, 1)
	assert.Equal(t, "local", api.createdVolumes[0].Driver)
	assert.Equal(t, "tmpfs", api.createdVolumes[0].DriverOpts["type"])
	assert.Equal(t, "platform", api.createdVolumes[0].Labels["team"])
}

func TestCreateVolumeMissingName(t *testing.T) {
	d := newVolumeDriver(newFakeJobAPI())
	got := runVolume(t, d, d.createVolume, map[string]any{})
	assert.Equal(t, dipper.ERROR, got.Labels["status"])
	assert.Contains(t, got.Labels["reason"], "missing volume name")
}

func TestCreateVolumeInvalidName(t *testing.T) {
	d := newVolumeDriver(newFakeJobAPI())
	got := runVolume(t, d, d.createVolume, map[string]any{"name": "bad/name"})
	assert.Equal(t, dipper.ERROR, got.Labels["status"])
	assert.Contains(t, got.Labels["reason"], "invalid volume name")
}

func TestCreateVolumeIgnoreIfExistsAlready(t *testing.T) {
	api := newFakeJobAPI()
	api.volumes["my-vol"] = true // volume already exists
	d := newVolumeDriver(api)
	got := runVolume(t, d, d.createVolume, map[string]any{"name": "my-vol", "ignore_if_exists": true})
	assert.Equal(t, "my-vol", got.Payload.(map[string]any)["volume_name"])
	assert.False(t, api.hasOp("createVolume:my-vol"), "must not create a volume that already exists")
}

func TestCreateVolumeIgnoreIfExistsMissing(t *testing.T) {
	api := newFakeJobAPI()
	d := newVolumeDriver(api)
	got := runVolume(t, d, d.createVolume, map[string]any{"name": "my-vol", "ignore_if_exists": true})
	assert.Equal(t, "my-vol", got.Payload.(map[string]any)["volume_name"])
	assert.True(t, api.hasOp("createVolume:my-vol"), "must create the volume when it does not exist")
}

func TestDeleteVolume(t *testing.T) {
	api := newFakeJobAPI()
	d := newVolumeDriver(api)
	got := runVolume(t, d, d.deleteVolume, map[string]any{"name": "my-vol", "force": true})
	assert.Equal(t, dipper.SUCCESS, got.Labels["status"])
	assert.True(t, api.hasOp("removeVolume:my-vol"))
	assert.True(t, api.removeVolumeForce["my-vol"])
}

func TestDeleteVolumeVolumeNameFallback(t *testing.T) {
	api := newFakeJobAPI()
	d := newVolumeDriver(api)
	got := runVolume(t, d, d.deleteVolume, map[string]any{"volume_name": "my-vol"})
	assert.Equal(t, dipper.SUCCESS, got.Labels["status"])
	assert.True(t, api.hasOp("removeVolume:my-vol"))
	assert.False(t, api.removeVolumeForce["my-vol"])
}

func TestDeleteVolumeMissingName(t *testing.T) {
	d := newVolumeDriver(newFakeJobAPI())
	got := runVolume(t, d, d.deleteVolume, map[string]any{})
	assert.Equal(t, dipper.ERROR, got.Labels["status"])
	assert.Contains(t, got.Labels["reason"], "missing volume name")
}

func TestDeleteVolumeInvalidName(t *testing.T) {
	d := newVolumeDriver(newFakeJobAPI())
	got := runVolume(t, d, d.deleteVolume, map[string]any{"name": "bad/name"})
	assert.Equal(t, dipper.ERROR, got.Labels["status"])
	assert.Contains(t, got.Labels["reason"], "invalid volume name")
}

func TestValidVolumeName(t *testing.T) {
	assert.True(t, validVolumeName("my-vol"))
	assert.True(t, validVolumeName("my.vol_1"))
	assert.False(t, validVolumeName("bad/name"))
	assert.False(t, validVolumeName("-bad"))
	assert.False(t, validVolumeName(""))
}

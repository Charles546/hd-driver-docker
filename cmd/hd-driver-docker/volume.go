// Copyright 2026 Chun Huang (Charles).

// This Source Code Form is dual-licensed.
// By default, this file is licensed under the GNU Affero General Public License v3.0.
// If you have a separate written commercial agreement, you may use this file under those terms instead.

package main

import (
	"fmt"

	"github.com/docker/docker/api/types/volume"
	"github.com/honeydipper/honeydipper/v4/pkg/dipper"
	"github.com/mitchellh/mapstructure"
)

// createVolume creates a named Docker volume, mirroring the podman driver's
// create_volume command. It accepts `name`, optional `ignore_if_exists`, and an
// optional `volume_spec` (volume.CreateOptions fields) decoded via mapstructure.
// The name is validated against Docker's volume-name charset.
func (d *dockerDriver) createVolume(msg *dipper.Message) {
	log := d.GetLogger()
	log.Debugf("[%s] create volume with payload %+v", d.Service, msg.Payload)
	msg = dipper.DeserializePayload(msg)
	ctx, cancel := d.GetContext(msg)
	defer cancel()
	ctx, api := d.getJobAPI(ctx, msg)

	var vspec any
	if v, ok := dipper.GetMapData(msg.Payload, "volume_spec"); ok {
		vspec = v
	}
	opts, err := parseVolumeSpec(vspec)
	if err != nil {
		panic(err)
	}
	if name, ok := dipper.GetMapDataStr(msg.Payload, "name"); ok {
		opts.Name = name
	}
	if opts.Name == "" {
		msg.Reply <- dipper.Message{Labels: map[string]string{"status": dipper.ERROR, "reason": "missing volume name"}}

		return
	}
	if !validVolumeName(opts.Name) {
		msg.Reply <- dipper.Message{Labels: map[string]string{
			"status": dipper.ERROR,
			"reason": fmt.Sprintf("invalid volume name %q: must match %s", opts.Name, volumeNamePattern),
		}}

		return
	}

	// Docker's volume create is not natively idempotent on name conflicts the
	// way podman's IgnoreIfExists is, so honor ignore_if_exists by checking for
	// an existing volume first and treating it as success.
	ignoreIfExists, _ := dipper.GetMapDataBool(msg.Payload, "ignore_if_exists")
	if ignoreIfExists {
		exists, err := api.VolumeExists(ctx, opts.Name)
		if err != nil {
			panic(err)
		}
		if exists {
			log.Infof("[%s] volume %s already exists, ignoring", d.Service, opts.Name)
			msg.Reply <- dipper.Message{Payload: map[string]any{"volume_name": opts.Name}}

			return
		}
	}

	if err := api.CreateVolume(ctx, opts); err != nil {
		panic(err)
	}
	msg.Reply <- dipper.Message{Payload: map[string]any{"volume_name": opts.Name}}
}

// volumeSpec is the parsed form of a create_volume `volume_spec` payload. It
// carries the volume.CreateOptions fields with explicit mapstructure tags so the
// snake_case keys used by the config bundle map cleanly onto the Docker types.
type volumeSpec struct {
	Driver     string            `mapstructure:"driver"`
	DriverOpts map[string]string `mapstructure:"driver_opts"`
	Labels     map[string]string `mapstructure:"labels"`
}

// parseVolumeSpec decodes a `volume_spec` payload (nil-safe) into Docker
// volume.CreateOptions. Unknown keys are ignored so future spec fields are
// forward-compatible.
func parseVolumeSpec(raw any) (volume.CreateOptions, error) {
	opts := volume.CreateOptions{}
	if raw == nil {
		return opts, nil
	}
	spec := volumeSpec{}
	if err := mapstructure.Decode(raw, &spec); err != nil {
		return opts, fmt.Errorf("failed to decode volume_spec: %w", err)
	}
	opts.Driver = spec.Driver
	opts.DriverOpts = spec.DriverOpts
	opts.Labels = spec.Labels

	return opts, nil
}

// deleteVolume removes a named Docker volume, mirroring the podman driver's
// delete_volume command. It accepts `name` (or `volume_name`, for compatibility
// with the podman config bundle) and an optional `force` flag.
func (d *dockerDriver) deleteVolume(msg *dipper.Message) {
	log := d.GetLogger()
	log.Debugf("[%s] delete volume with payload %+v", d.Service, msg.Payload)
	msg = dipper.DeserializePayload(msg)
	ctx, cancel := d.GetContext(msg)
	defer cancel()
	ctx, api := d.getJobAPI(ctx, msg)

	volumeName, ok := dipper.GetMapDataStr(msg.Payload, "name")
	if !ok {
		// podman's delete_volume uses `volume_name`; accept it as a fallback so
		// the same config bundle key works against either driver.
		volumeName, _ = dipper.GetMapDataStr(msg.Payload, "volume_name")
	}
	if volumeName == "" {
		msg.Reply <- dipper.Message{Labels: map[string]string{"status": dipper.ERROR, "reason": "missing volume name"}}

		return
	}
	if !validVolumeName(volumeName) {
		msg.Reply <- dipper.Message{Labels: map[string]string{
			"status": dipper.ERROR,
			"reason": fmt.Sprintf("invalid volume name %q: must match %s", volumeName, volumeNamePattern),
		}}

		return
	}

	force, _ := dipper.GetMapDataBool(msg.Payload, "force")
	if err := api.RemoveVolume(ctx, volumeName, force); err != nil {
		panic(err)
	}
	msg.Reply <- dipper.Message{Labels: map[string]string{"status": dipper.SUCCESS}}
}

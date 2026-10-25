// Copyright 2026 Chun Huang (Charles).

// This Source Code Form is dual-licensed.
// By default, this file is licensed under the GNU Affero General Public License v3.0.
// If you have a separate written commercial agreement, you may use this file under those terms instead.

package main

import (
	"fmt"

	"github.com/docker/docker/api/types/container"
	"github.com/docker/docker/api/types/mount"
	"github.com/docker/docker/api/types/network"
	"github.com/docker/docker/api/types/strslice"
	"github.com/honeydipper/honeydipper/v4/pkg/dipper"
	"github.com/mitchellh/mapstructure"
)

// containerSpec is the parsed form of one entry in a job's `containers` list.
type containerSpec struct {
	Name            string
	Image           string
	ImagePullPolicy string
	Config          *container.Config
	HostConfig      *container.HostConfig
	Networking      *network.NetworkingConfig
}

// containerSpecScalar carries the scalar fields of a container spec that map
// directly onto Docker config/host-config fields. The complex fields (command,
// env, labels, mounts, binds) are parsed separately.
type containerSpecScalar struct {
	Name            string  `mapstructure:"name"`
	Image           string  `mapstructure:"image"`
	ImagePullPolicy string  `mapstructure:"imagePullPolicy"`
	Workdir         string  `mapstructure:"workdir"`
	User            string  `mapstructure:"user"`
	Hostname        string  `mapstructure:"hostname"`
	StopSignal      string  `mapstructure:"stop_signal"`
	Tty             bool    `mapstructure:"tty"`
	Privileged      bool    `mapstructure:"privileged"`
	Network         string  `mapstructure:"network"`
	Memory          int64   `mapstructure:"memory"`
	NanoCPUs        int64   `mapstructure:"nano_cpus"`
	CPUs            float64 `mapstructure:"cpus"`
}

// parseContainerSpec maps a generic container spec map (as carried by the job
// payload) onto the Docker Engine container.Config / HostConfig / NetworkingConfig
// shapes. Unknown keys are ignored so future spec fields are forward-compatible.
func parseContainerSpec(raw map[string]any) (containerSpec, error) {
	spec := containerSpec{
		Config:     &container.Config{},
		HostConfig: &container.HostConfig{},
		Networking: &network.NetworkingConfig{},
	}

	var scalar containerSpecScalar
	if err := mapstructure.Decode(raw, &scalar); err != nil {
		return spec, fmt.Errorf("failed to decode container spec: %w", err)
	}
	if scalar.Image == "" {
		return spec, fmt.Errorf("container spec is missing required field: image")
	}
	spec.Name = scalar.Name
	spec.Image = scalar.Image
	spec.ImagePullPolicy = scalar.ImagePullPolicy

	spec.Config.Image = scalar.Image
	spec.Config.WorkingDir = scalar.Workdir
	spec.Config.User = scalar.User
	spec.Config.Hostname = scalar.Hostname
	spec.Config.StopSignal = scalar.StopSignal
	spec.Config.Tty = scalar.Tty
	spec.HostConfig.Privileged = scalar.Privileged
	if scalar.Network != "" {
		spec.HostConfig.NetworkMode = container.NetworkMode(scalar.Network)
	}
	spec.HostConfig.Memory = scalar.Memory
	if scalar.NanoCPUs != 0 {
		spec.HostConfig.NanoCPUs = scalar.NanoCPUs
	} else if scalar.CPUs != 0 {
		spec.HostConfig.NanoCPUs = int64(scalar.CPUs * 1e9)
	}

	if v, ok := raw["command"]; ok && v != nil {
		cmd, err := toStringSlice(v)
		if err != nil {
			return spec, err
		}
		spec.Config.Cmd = strslice.StrSlice(cmd)
	}
	if v, ok := raw["entrypoint"]; ok && v != nil {
		ep, err := toStringSlice(v)
		if err != nil {
			return spec, err
		}
		spec.Config.Entrypoint = strslice.StrSlice(ep)
	}
	if v, ok := raw["env"]; ok && v != nil {
		env, err := toEnvSlice(v)
		if err != nil {
			return spec, err
		}
		spec.Config.Env = env
	}
	if v, ok := raw["labels"]; ok && v != nil {
		labels, err := toStringMap(v)
		if err != nil {
			return spec, err
		}
		spec.Config.Labels = labels
	}
	if v, ok := raw["binds"]; ok && v != nil {
		binds, err := toStringSlice(v)
		if err != nil {
			return spec, err
		}
		spec.HostConfig.Binds = binds
	}
	if v, ok := raw["mounts"]; ok && v != nil {
		mounts, err := toMounts(v)
		if err != nil {
			return spec, err
		}
		spec.HostConfig.Mounts = mounts
	}

	return spec, nil
}

// toStringSlice converts a value into a list of strings.
func toStringSlice(v any) ([]string, error) {
	switch t := v.(type) {
	case []any:
		ret := make([]string, 0, len(t))
		for _, e := range t {
			ret = append(ret, fmt.Sprintf("%v", e))
		}

		return ret, nil
	case []string:
		return t, nil
	case string:
		return []string{t}, nil
	}

	return nil, fmt.Errorf("expected a list of strings, got %T", v)
}

// toEnvSlice converts a value into the ENV list Docker expects (KEY=VALUE).
func toEnvSlice(v any) ([]string, error) {
	switch t := v.(type) {
	case []any:
		ret := make([]string, 0, len(t))
		for _, e := range t {
			ret = append(ret, fmt.Sprintf("%v", e))
		}

		return ret, nil
	case []string:
		return t, nil
	case map[string]any:
		ret := make([]string, 0, len(t))
		for k, val := range t {
			ret = append(ret, fmt.Sprintf("%s=%v", k, val))
		}

		return ret, nil
	}

	return nil, fmt.Errorf("env must be a list of KEY=VALUE or a map, got %T", v)
}

// toStringMap converts a value into a string map.
func toStringMap(v any) (map[string]string, error) {
	switch t := v.(type) {
	case map[string]any:
		ret := map[string]string{}
		for k, val := range t {
			ret[k] = fmt.Sprintf("%v", val)
		}

		return ret, nil
	case map[string]string:
		return t, nil
	}

	return nil, fmt.Errorf("expected a string map, got %T", v)
}

// toMounts converts a value into the Docker mount list.
func toMounts(v any) ([]mount.Mount, error) {
	items, ok := v.([]any)
	if !ok {
		return nil, fmt.Errorf("mounts must be a list, got %T", v)
	}
	ret := make([]mount.Mount, 0, len(items))
	for _, item := range items {
		m, ok := item.(map[string]any)
		if !ok {
			return nil, fmt.Errorf("mount entry must be a map, got %T", item)
		}
		mm := mount.Mount{
			Type:   mount.TypeVolume,
			Target: mustStr(m, "target"),
		}
		if s, ok := dipper.GetMapDataStr(m, "source"); ok {
			mm.Source = s
		}
		if t, ok := dipper.GetMapDataStr(m, "type"); ok {
			mm.Type = mount.Type(t)
		}
		if ro, ok := dipper.GetMapDataBool(m, "read_only"); ok {
			mm.ReadOnly = ro
		}
		ret = append(ret, mm)
	}

	return ret, nil
}

// mustStr extracts a required string field from a map.
func mustStr(m map[string]any, key string) string {
	if v, ok := dipper.GetMapDataStr(m, key); ok {
		return v
	}

	return ""
}

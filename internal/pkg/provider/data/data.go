// This Source Code Form is subject to the terms of the Mozilla Public
// License, v. 2.0. If a copy of the MPL was not distributed with this
// file, You can obtain one at http://mozilla.org/MPL/2.0/.

// Package data defines the machine class schema for the OpenStack provider.
package data

import (
	_ "embed"
)

//go:embed schema.json
var Schema []byte

// Data and schema.json should be in sync.
type Data struct {
	Flavor           string           `yaml:"flavor"`
	Network          string           `yaml:"network"`
	SecurityGroups   []string         `yaml:"security_groups,omitempty"`
	DiskSize         int              `yaml:"disk_size"`
	VolumeType       string           `yaml:"volume_type,omitempty"`
	AvailabilityZone string           `yaml:"availability_zone,omitempty"`
	Architecture     string           `yaml:"architecture"`
	ServerGroup      string           `yaml:"server_group,omitempty"`
	Metadata         map[string]string `yaml:"metadata,omitempty"`
	AdditionalDisks  []AdditionalDisk `yaml:"additional_disks,omitempty"`
	AdditionalNICs   []AdditionalNIC  `yaml:"additional_nics,omitempty"`
}

// AdditionalNIC describes an extra Neutron port attached to the machine.
type AdditionalNIC struct {
	Network         string   `yaml:"network"`
	SecurityGroups  []string `yaml:"security_groups,omitempty"`
}

// AdditionalDisk describes an extra Cinder volume attached to the machine.
type AdditionalDisk struct {
	SizeGB     int    `yaml:"size_gb"`
	VolumeType string `yaml:"volume_type,omitempty"`
}

// This Source Code Form is subject to the terms of the Mozilla Public
// License, v. 2.0. If a copy of the MPL was not distributed with this
// file, You can obtain one at http://mozilla.org/MPL/2.0/.

package provider

import (
	"reflect"
	"testing"
)

func TestFirmwareImage(t *testing.T) {
	for _, tc := range []struct {
		name      string
		firmware  string
		wantKey   string
		wantProps map[string]string
	}{
		{
			name:      "empty firmware keeps key and sets no properties",
			firmware:  "",
			wantKey:   "sha256-abc",
			wantProps: nil,
		},
		{
			name:      "uefi suffixes key and sets hw_firmware_type",
			firmware:  "uefi",
			wantKey:   "sha256-abc-uefi",
			wantProps: map[string]string{"hw_firmware_type": "uefi"},
		},
		{
			name:      "bios suffixes key and sets hw_firmware_type",
			firmware:  "bios",
			wantKey:   "sha256-abc-bios",
			wantProps: map[string]string{"hw_firmware_type": "bios"},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			gotKey, gotProps := firmwareImage("sha256-abc", tc.firmware)

			if gotKey != tc.wantKey {
				t.Errorf("key = %q, want %q", gotKey, tc.wantKey)
			}

			if !reflect.DeepEqual(gotProps, tc.wantProps) {
				t.Errorf("props = %v, want %v", gotProps, tc.wantProps)
			}
		})
	}
}

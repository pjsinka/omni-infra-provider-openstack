// This Source Code Form is subject to the terms of the Mozilla Public
// License, v. 2.0. If a copy of the MPL was not distributed with this
// file, You can obtain one at http://mozilla.org/MPL/2.0/.

package data

import "testing"

func TestValidateFirmware(t *testing.T) {
	for _, firmware := range []string{"", "uefi", "bios"} {
		if err := ValidateFirmware(firmware); err != nil {
			t.Errorf("ValidateFirmware(%q) = %v, want nil", firmware, err)
		}
	}

	for _, firmware := range []string{"UEFI", "efi", "BIOS", "secure-boot", "uefi ", "uefi,bios"} {
		if err := ValidateFirmware(firmware); err == nil {
			t.Errorf("ValidateFirmware(%q) = nil, want error", firmware)
		}
	}
}

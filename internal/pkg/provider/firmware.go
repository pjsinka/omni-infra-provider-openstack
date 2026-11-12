// This Source Code Form is subject to the terms of the Mozilla Public
// License, v. 2.0. If a copy of the MPL was not distributed with this
// file, You can obtain one at http://mozilla.org/MPL/2.0/.

package provider

// firmwareImage returns the Glance storage key and image properties for the
// given machine class firmware setting. An empty firmware leaves the key
// untouched and sets no properties (Nova default boot mode); otherwise the
// firmware becomes an image property and part of the image name, because
// BIOS and UEFI machines need separate Glance images.
func firmwareImage(storageKey, firmware string) (string, map[string]string) {
	if firmware == "" {
		return storageKey, nil
	}

	return storageKey + "-" + firmware, map[string]string{"hw_firmware_type": firmware}
}

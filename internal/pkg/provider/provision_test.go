// This Source Code Form is subject to the terms of the Mozilla Public
// License, v. 2.0. If a copy of the MPL was not distributed with this
// file, You can obtain one at http://mozilla.org/MPL/2.0/.

package provider

import (
	"strings"
	"testing"
)

func TestValidateRequestName(t *testing.T) {
	for _, name := range []string{"talos-default-control-planes-qjnd62", strings.Repeat("a", 255)} {
		if err := validateRequestName(name); err != nil {
			t.Errorf("validateRequestName(len %d) = %v, want nil", len(name), err)
		}
	}

	// 256 characters: rejected locally before Nova fails the server create
	// with a hard error the provision loop would retry forever.
	if err := validateRequestName(strings.Repeat("a", 256)); err == nil {
		t.Error("validateRequestName(256 chars) = nil, want error")
	}
}

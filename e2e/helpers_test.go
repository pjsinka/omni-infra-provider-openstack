// This Source Code Form is subject to the terms of the Mozilla Public
// License, v. 2.0. If a copy of the MPL was not distributed with this
// file, You can obtain one at http://mozilla.org/MPL/2.0/.

//go:build e2e

package e2e

import (
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/blang/semver/v4"
)

// randomSuffix returns 8 lowercase hex characters, unique enough to isolate
// one e2e run's Omni/OpenStack resources from any other run's leftovers.
func randomSuffix() (string, error) {
	b := make([]byte, 4)

	if _, err := rand.Read(b); err != nil {
		return "", fmt.Errorf("reading random bytes: %w", err)
	}

	return hex.EncodeToString(b), nil
}

// nameReplacer builds the single-pass replacer for the testdata templates.
// Order matters: the most specific names must come first so that
// "omni-e2e-mc" and "omni-e2e-control-planes" are not consumed by the bare
// "omni-e2e" cluster-name rule.
//
// Naming contract (validated in Runs 16-22):
//   - cluster id:      omni-e2e-<suffix>
//   - machineclass id: omni-e2e-mc-<suffix>
//   - machineset id:   omni-e2e-<suffix>-control-planes (Run 19: Omni
//     requires <cluster>-control-planes for a control plane machine set)
//   - security group:  e2e-sg-<suffix>
func nameReplacer(clusterName, mcName, msName, sgName, talosVersion, kubernetesVersion string) *strings.Replacer {
	return strings.NewReplacer(
		"omni-e2e-mc", mcName,
		"omni-e2e-control-planes", msName,
		"omni-e2e", clusterName,
		"e2e-sg", sgName,
		"__TALOS_VERSION__", talosVersion,
		"__KUBERNETES_VERSION__", kubernetesVersion,
	)
}

// renderTemplate reads a testdata template and applies the replacer. It
// returns the rendered content; writeTemp persists it for omnictl apply.
func renderTemplate(t *testing.T, src string, r *strings.Replacer) string {
	t.Helper()

	raw, err := os.ReadFile(filepath.Join("testdata", src))
	if err != nil {
		t.Fatalf("reading template %s: %v", src, err)
	}

	return r.Replace(string(raw))
}

// writeTemp writes content to a file in t.TempDir() and returns its path.
func writeTemp(t *testing.T, name, content string) string {
	t.Helper()

	path := filepath.Join(t.TempDir(), name)
	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		t.Fatalf("writing %s: %v", name, err)
	}

	return path
}

// selectVersion picks the version id to use from the ids an Omni instance
// reports. Stable (no prerelease) versions are preferred over prereleases;
// within the preference group the maximum semver wins. Non-semver ids are
// ignored. It returns the original id string (Omni validates version
// strings against its own list, so the exact reported id must be used).
func selectVersion(ids []string) (string, error) {
	var (
		best, bestStable     semver.Version
		bestID, bestStableID string
		haveAny, haveStable  bool
	)

	for _, id := range ids {
		v, err := semver.ParseTolerant(id)
		if err != nil {
			continue
		}

		if !haveAny || v.GT(best) {
			best, bestID, haveAny = v, id, true
		}

		if len(v.Pre) == 0 && (!haveStable || v.GT(bestStable)) {
			bestStable, bestStableID, haveStable = v, id, true
		}
	}

	switch {
	case haveStable:
		return bestStableID, nil
	case haveAny:
		return bestID, nil
	default:
		return "", fmt.Errorf("no parseable versions in %d ids", len(ids))
	}
}

func TestSelectVersion(t *testing.T) {
	tests := []struct {
		name string
		ids  []string
		want string
	}{
		{
			name: "stable preferred over newer prerelease",
			ids:  []string{"v1.9.0", "v1.10.5", "v1.11.0-beta.1"},
			want: "v1.10.5",
		},
		{
			name: "max stable overall",
			ids:  []string{"v1.33.0", "v1.34.0", "v1.32.9"},
			want: "v1.34.0",
		},
		{
			name: "falls back to max prerelease when no stable exists",
			ids:  []string{"v1.34.0-beta.1", "v1.34.0-beta.2"},
			want: "v1.34.0-beta.2",
		},
		{
			name: "ignores non-semver ids",
			ids:  []string{"latest", "v1.33.0", "v1.34.0"},
			want: "v1.34.0",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := selectVersion(tt.ids)
			if err != nil {
				t.Fatalf("selectVersion(%v) failed: %v", tt.ids, err)
			}

			if got != tt.want {
				t.Errorf("selectVersion(%v) = %q, want %q", tt.ids, got, tt.want)
			}
		})
	}

	if _, err := selectVersion(nil); err == nil {
		t.Error("selectVersion(nil) should error, got nil")
	}

	if _, err := selectVersion([]string{"latest", "nightly"}); err == nil {
		t.Error("selectVersion with only non-semver ids should error, got nil")
	}
}

func TestRenderTemplates(t *testing.T) {
	const (
		suffix            = "ab12cd"
		talosVersion      = "v9.9.9"
		kubernetesVersion = "v1.2.3"
	)

	clusterName := "omni-e2e-" + suffix
	mcName := "omni-e2e-mc-" + suffix
	msName := clusterName + "-control-planes"
	sgName := "e2e-sg-" + suffix

	r := nameReplacer(clusterName, mcName, msName, sgName, talosVersion, kubernetesVersion)

	tests := []struct {
		src     string
		content []string
	}{
		{
			src: "cluster.template.yaml",
			content: []string{
				"id: omni-e2e-ab12cd",
				"talosversion: v9.9.9",
				"kubernetesversion: v1.2.3",
			},
		},
		{
			src: "machineclass.yaml",
			content: []string{
				"id: omni-e2e-mc-ab12cd",
				"security_groups: [e2e-sg-ab12cd]",
			},
		},
		{
			src: "machineset.yaml",
			content: []string{
				// Run 19: machine set id must be <cluster>-control-planes.
				"id: omni-e2e-ab12cd-control-planes",
				// Run 16: cluster label must reference the cluster id.
				"omni.sidero.dev/cluster: omni-e2e-ab12cd",
				// Run 18: role label must be present (empty value).
				"omni.sidero.dev/role-controlplane:",
				// Allocation must reference the suffixed machineclass.
				"name: omni-e2e-mc-ab12cd",
			},
		},
	}

	for _, tt := range tests {
		t.Run(tt.src, func(t *testing.T) {
			rendered := renderTemplate(t, tt.src, r)

			if strings.Contains(rendered, "__TALOS_VERSION__") || strings.Contains(rendered, "__KUBERNETES_VERSION__") {
				t.Errorf("%s: version placeholder not replaced", tt.src)
			}

			for _, want := range tt.content {
				if !strings.Contains(rendered, want) {
					t.Errorf("%s: missing %q in rendered template", tt.src, want)
				}
			}
		})
	}
}

func TestWriteTemp(t *testing.T) {
	path := writeTemp(t, "cluster.yaml", "metadata:\n    id: test\n")

	got, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("reading back %s: %v", path, err)
	}

	if string(got) != "metadata:\n    id: test\n" {
		t.Errorf("writeTemp content mismatch: %q", string(got))
	}
}

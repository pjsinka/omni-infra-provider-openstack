// This Source Code Form is subject to the terms of the Mozilla Public
// License, v. 2.0. If a copy of the MPL was not distributed with this
// file, You can obtain one at http://mozilla.org/MPL/2.0/.

package openstack

import (
	"encoding/json"
	"net/http"
	"strings"
	"testing"

	th "github.com/gophercloud/gophercloud/testhelper"
)

func TestResolveNetwork(t *testing.T) {
	t.Run("resolves name to id", func(t *testing.T) {
		client := newTestClient(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if r.URL.Path != "/networks" {
				t.Errorf("unexpected path %q", r.URL.Path)
			}

			if name := r.URL.Query().Get("name"); name != "tenant" {
				t.Errorf("unexpected name filter %q", name)
			}

			w.Header().Set("Content-Type", "application/json")
			_ = json.NewEncoder(w).Encode(map[string]any{
				"networks": []map[string]any{{"id": "net-uuid", "name": "tenant"}},
			})
		}))

		id, err := client.resolveNetwork("tenant")
		th.AssertNoErr(t, err)
		th.AssertEquals(t, "net-uuid", id)
	})

	t.Run("passes through when no match", func(t *testing.T) {
		client := newTestClient(t, http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			w.Header().Set("Content-Type", "application/json")
			_ = json.NewEncoder(w).Encode(map[string]any{"networks": []map[string]any{}})
		}))

		id, err := client.resolveNetwork("net-uuid")
		th.AssertNoErr(t, err)
		th.AssertEquals(t, "net-uuid", id)
	})

	t.Run("errors when ambiguous", func(t *testing.T) {
		client := newTestClient(t, http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			w.Header().Set("Content-Type", "application/json")
			_ = json.NewEncoder(w).Encode(map[string]any{
				"networks": []map[string]any{
					{"id": "net-1", "name": "tenant"},
					{"id": "net-2", "name": "tenant"},
				},
			})
		}))

		_, err := client.resolveNetwork("tenant")
		if err == nil {
			t.Fatal("expected ambiguity error, got nil")
		}

		if !strings.Contains(err.Error(), "ambiguous") {
			t.Fatalf("expected ambiguous error, got %v", err)
		}
	})
}

func TestResolveFlavor(t *testing.T) {
	t.Run("resolves name to id", func(t *testing.T) {
		client := newTestClient(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if r.URL.Path != "/flavors/detail" {
				t.Errorf("unexpected path %q", r.URL.Path)
			}

			w.Header().Set("Content-Type", "application/json")
			_ = json.NewEncoder(w).Encode(map[string]any{
				"flavors": []map[string]any{{
					"id": "flav-uuid", "name": "m1.large", "vcpus": 2, "ram": 4096, "disk": 20,
				}},
			})
		}))

		id, err := client.resolveFlavor("m1.large")
		th.AssertNoErr(t, err)
		th.AssertEquals(t, "flav-uuid", id)
	})

	t.Run("passes through when no name match", func(t *testing.T) {
		client := newTestClient(t, http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			w.Header().Set("Content-Type", "application/json")
			_ = json.NewEncoder(w).Encode(map[string]any{
				"flavors": []map[string]any{{"id": "other", "name": "other-flavor"}},
			})
		}))

		id, err := client.resolveFlavor("flav-uuid")
		th.AssertNoErr(t, err)
		th.AssertEquals(t, "flav-uuid", id)
	})
}

func TestUUIDLike(t *testing.T) {
	for _, tc := range []struct {
		in   string
		want bool
	}{
		{"123e4567-e89b-12d3-a456-426614174000", true},
		{"123e4567e89b-12d3-a456-426614174000", false}, // dash in wrong position
		{"123e4567-e89b-12d3-a456-42661417400z", false}, // non-hex character
		{"123e4567-e89b-12d3-a456-4266141740", false},   // wrong length
	} {
		if got := uuidLike(tc.in); got != tc.want {
			t.Errorf("uuidLike(%q) = %v, want %v", tc.in, got, tc.want)
		}
	}
}

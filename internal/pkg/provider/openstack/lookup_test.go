// This Source Code Form is subject to the terms of the Mozilla Public
// License, v. 2.0. If a copy of the MPL was not distributed with this
// file, You can obtain one at http://mozilla.org/MPL/2.0/.

package openstack

import (
	"context"
	"encoding/json"
	"net/http"
	"strings"
	"testing"

	th "github.com/gophercloud/gophercloud/testhelper"
)

func TestServerStatus(t *testing.T) {
	t.Run("returns nova status", func(t *testing.T) {
		client := newTestClient(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if r.URL.Path != "/servers/srv-uuid" {
				t.Errorf("unexpected path %q", r.URL.Path)
			}

			w.Header().Set("Content-Type", "application/json")
			_ = json.NewEncoder(w).Encode(map[string]any{
				"server": map[string]any{"id": "srv-uuid", "status": "ACTIVE"},
			})
		}))

		status, err := client.ServerStatus(context.Background(), "srv-uuid")
		th.AssertNoErr(t, err)
		th.AssertEquals(t, "ACTIVE", status)
	})

	t.Run("deleted when server is gone", func(t *testing.T) {
		client := newTestClient(t, http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			w.WriteHeader(http.StatusNotFound)
			_ = json.NewEncoder(w).Encode(map[string]any{
				"itemNotFound": map[string]any{"message": "not found", "code": 404},
			})
		}))

		status, err := client.ServerStatus(context.Background(), "srv-uuid")
		th.AssertNoErr(t, err)
		th.AssertEquals(t, "DELETED", status)
	})
}

func TestAvailabilityZones(t *testing.T) {
	t.Run("returns only available zones", func(t *testing.T) {
		client := newTestClient(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if r.URL.Path != "/os-availability-zone" {
				t.Errorf("unexpected path %q", r.URL.Path)
			}

			w.Header().Set("Content-Type", "application/json")
			_ = json.NewEncoder(w).Encode(map[string]any{
				"availabilityZoneInfo": []map[string]any{
					{"zoneName": "az-1", "zoneState": map[string]any{"available": true}},
					{"zoneName": "az-2", "zoneState": map[string]any{"available": false}},
				},
			})
		}))

		zones, err := client.AvailabilityZones()
		th.AssertNoErr(t, err)
		th.AssertDeepEquals(t, []string{"az-1"}, zones)
	})

	t.Run("returns sorted zones", func(t *testing.T) {
		client := newTestClient(t, http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			w.Header().Set("Content-Type", "application/json")
			_ = json.NewEncoder(w).Encode(map[string]any{
				"availabilityZoneInfo": []map[string]any{
					{"zoneName": "nova-b", "zoneState": map[string]any{"available": true}},
					{"zoneName": "nova-a", "zoneState": map[string]any{"available": true}},
					{"zoneName": "nova-c", "zoneState": map[string]any{"available": false}},
				},
			})
		}))

		zones, err := client.AvailabilityZones()
		th.AssertNoErr(t, err)
		th.AssertDeepEquals(t, []string{"nova-a", "nova-b"}, zones)
	})
}

func TestImageIDByName(t *testing.T) {
	t.Run("resolves image name to id", func(t *testing.T) {
		client := newTestClient(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if r.URL.Path != "/images" {
				t.Errorf("unexpected path %q", r.URL.Path)
			}

			if name := r.URL.Query().Get("name"); name != "key" {
				t.Errorf("unexpected name filter %q", name)
			}

			w.Header().Set("Content-Type", "application/json")
			_ = json.NewEncoder(w).Encode(map[string]any{
				"images": []map[string]any{{"id": "img-uuid", "name": "key", "status": "active"}},
			})
		}))

		id, err := client.ImageIDByName("key")
		th.AssertNoErr(t, err)
		th.AssertEquals(t, "img-uuid", id)
	})

	t.Run("errors when image not found", func(t *testing.T) {
		client := newTestClient(t, http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			w.Header().Set("Content-Type", "application/json")
			_ = json.NewEncoder(w).Encode(map[string]any{"images": []map[string]any{}})
		}))

		_, err := client.ImageIDByName("key")
		if err == nil {
			t.Fatal("expected not-found error, got nil")
		}

		if !strings.Contains(err.Error(), "not found") {
			t.Fatalf("expected not-found error, got %v", err)
		}
	})
}

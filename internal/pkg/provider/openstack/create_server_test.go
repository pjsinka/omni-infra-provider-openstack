// This Source Code Form is subject to the terms of the Mozilla Public
// License, v. 2.0. If a copy of the MPL was not distributed with this
// file, You can obtain one at http://mozilla.org/MPL/2.0/.

package openstack

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"net/http"
	"strings"
	"sync"
	"testing"

	th "github.com/gophercloud/gophercloud/testhelper"
	"go.uber.org/zap"
)

// deleteTracker records port deletions across handler goroutines.
type deleteTracker struct {
	mu      sync.Mutex
	deleted map[string]bool
}

func newDeleteTracker() *deleteTracker {
	return &deleteTracker{deleted: map[string]bool{}}
}

func (d *deleteTracker) record(portID string) {
	d.mu.Lock()
	defer d.mu.Unlock()

	d.deleted[portID] = true
}

func (d *deleteTracker) assertDeleted(t *testing.T, portID string) {
	t.Helper()

	d.mu.Lock()
	defer d.mu.Unlock()

	if !d.deleted[portID] {
		t.Errorf("port %q was not deleted, deleted: %v", portID, d.deleted)
	}
}

func (d *deleteTracker) assertNotDeleted(t *testing.T, portID string) {
	t.Helper()

	d.mu.Lock()
	defer d.mu.Unlock()

	if d.deleted[portID] {
		t.Errorf("port %q must not have been deleted", portID)
	}
}

func TestCreateServer_FullRequest(t *testing.T) {
	client := newTestClient(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")

		switch {
		case r.Method == http.MethodGet && r.URL.Path == "/networks":
			switch r.URL.Query().Get("name") {
			case "tenant":
				_ = json.NewEncoder(w).Encode(map[string]any{"networks": []map[string]any{{"id": "net-uuid", "name": "tenant"}}})
			case "storage-net":
				_ = json.NewEncoder(w).Encode(map[string]any{"networks": []map[string]any{{"id": "net2-uuid", "name": "storage-net"}}})
			default:
				t.Errorf("unexpected network lookup %q", r.URL.RawQuery)
			}

		case r.Method == http.MethodPost && r.URL.Path == "/ports":
			body := serverBody(t, r)
			port := body["port"].(map[string]any)

			switch port["network_id"] {
			case "net-uuid":
				th.AssertEquals(t, "machine-req", port["name"])
				th.AssertDeepEquals(t, []any{"sec-uuid"}, port["security_groups"])

				w.WriteHeader(http.StatusCreated)
				_ = json.NewEncoder(w).Encode(map[string]any{"port": map[string]any{"id": "port-main", "name": "machine-req"}})
			case "net2-uuid":
				th.AssertEquals(t, "machine-req-nic-0", port["name"])
				th.AssertDeepEquals(t, []any{"sec-storage"}, port["security_groups"])

				w.WriteHeader(http.StatusCreated)
				_ = json.NewEncoder(w).Encode(map[string]any{"port": map[string]any{"id": "port-nic-0", "name": "machine-req-nic-0"}})
			default:
				t.Errorf("unexpected port create for network %v", port["network_id"])
			}

		case r.Method == http.MethodGet && r.URL.Path == "/security-groups":
			_ = json.NewEncoder(w).Encode(map[string]any{
				"security_groups": []map[string]any{
					{"id": "sec-uuid", "name": "e2e-sg"},
					{"id": "sec-storage", "name": "storage-sg"},
				},
			})

		case r.Method == http.MethodGet && r.URL.Path == "/flavors/detail":
			_ = json.NewEncoder(w).Encode(map[string]any{
				"flavors": []map[string]any{{"id": "flav-uuid", "name": "m1.large", "vcpus": 2, "ram": 4096, "disk": 20}},
			})

		case r.Method == http.MethodGet && r.URL.Path == "/os-server-groups":
			_ = json.NewEncoder(w).Encode(map[string]any{
				"server_groups": []map[string]any{{"id": "123e4567-e89b-12d3-a456-426614174001", "name": "anti-affinity", "policies": []string{"anti-affinity"}}},
			})

		case r.Method == http.MethodPost && r.URL.Path == "/servers":
			th.AssertEquals(t, "2.67", r.Header.Get("X-OpenStack-Nova-API-Version"))

			body := serverBody(t, r)
			server := body["server"].(map[string]any)

			th.AssertEquals(t, "machine-req", server["name"])
			th.AssertEquals(t, "flav-uuid", server["flavorRef"])
			th.AssertEquals(t, "az-1", server["availability_zone"])
			th.AssertEquals(t, true, server["config_drive"])
			th.AssertDeepEquals(t, map[string]any{"managed-by": "omni"}, server["metadata"])

			// Boot-from-volume still carries an empty imageRef — pin the
			// actual wire format (Nova accepts this with BDM v2 set).
			th.AssertEquals(t, "", server["imageRef"])

			nets := server["networks"].([]any)
			net0, ok0 := nets[0].(map[string]any)
			net1, ok1 := nets[1].(map[string]any)
			if !ok0 || !ok1 {
				t.Fatalf("unexpected networks %v", nets)
			}

			th.AssertEquals(t, "port-main", net0["port"])
			th.AssertEquals(t, "port-nic-0", net1["port"])

			userData, err := base64.StdEncoding.DecodeString(server["user_data"].(string))
			if err != nil {
				t.Fatalf("decoding user data: %v", err)
			}

			th.AssertEquals(t, "#cloud-config\n", string(userData))

			bdm, ok := server["block_device_mapping_v2"].([]any)
			if !ok || len(bdm) != 2 {
				t.Fatalf("unexpected block device mapping %v", server["block_device_mapping_v2"])
			}

			boot, _ := bdm[0].(map[string]any)
			th.AssertEquals(t, "image", boot["source_type"])
			th.AssertEquals(t, "img-uuid", boot["uuid"])
			th.AssertEquals(t, float64(0), boot["boot_index"])
			th.AssertEquals(t, true, boot["delete_on_termination"])
			th.AssertEquals(t, "volume", boot["destination_type"])
			th.AssertEquals(t, float64(20), boot["volume_size"])
			th.AssertEquals(t, "ssd", boot["volume_type"])

			if _, ok := boot["tag"]; ok {
				t.Error("boot device must not carry a tag")
			}

			data, _ := bdm[1].(map[string]any)
			th.AssertEquals(t, "blank", data["source_type"])
			th.AssertEquals(t, float64(-1), data["boot_index"])
			th.AssertEquals(t, true, data["delete_on_termination"])
			th.AssertEquals(t, "volume", data["destination_type"])
			th.AssertEquals(t, float64(5), data["volume_size"])
			th.AssertEquals(t, "data-0", data["tag"])
			th.AssertEquals(t, "hdd", data["volume_type"])

			// gophercloud's scheduler-hints extension merges hints at the
			// top level under the "os:"-prefixed key — that is the key
			// Nova's create API actually reads.
			hints, ok := body["os:scheduler_hints"].(map[string]any)
			if !ok {
				t.Fatalf("expected top-level os:scheduler_hints, got %v", body)
			}

			th.AssertEquals(t, "123e4567-e89b-12d3-a456-426614174001", hints["group"])

			w.WriteHeader(http.StatusAccepted)
			_ = json.NewEncoder(w).Encode(map[string]any{"server": map[string]any{"id": "srv-uuid", "status": "BUILD"}})

		default:
			t.Errorf("unexpected request %s %s", r.Method, r.URL)
		}
	}))

	serverID, err := client.CreateServer(context.Background(), zap.NewNop(), ServerRequest{
		Name:             "machine-req",
		ImageID:          "img-uuid",
		Flavor:           "m1.large",
		Network:          "tenant",
		SecurityGroups:   []string{"e2e-sg"},
		DiskSizeGB:       20,
		VolumeType:       "ssd",
		AvailabilityZone: "az-1",
		ServerGroup:      "anti-affinity",
		UserData:         "#cloud-config\n",
		ConfigDrive:      true,
		Metadata:         map[string]string{"managed-by": "omni"},
		AdditionalDisks:  []AdditionalDisk{{SizeGB: 5, VolumeType: "hdd"}},
		AdditionalNICs:   []AdditionalNIC{{Network: "storage-net", SecurityGroups: []string{"storage-sg"}}},
	})
	th.AssertNoErr(t, err)
	th.AssertEquals(t, "srv-uuid", serverID)
}

func TestCreateServer_MinimalRequest(t *testing.T) {
	client := newTestClient(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")

		switch {
		case r.Method == http.MethodGet && r.URL.Path == "/networks":
			th.AssertEquals(t, "net-uuid", r.URL.Query().Get("name"))
			_ = json.NewEncoder(w).Encode(map[string]any{"networks": []map[string]any{}})

		case r.Method == http.MethodGet && r.URL.Path == "/flavors/detail":
			_ = json.NewEncoder(w).Encode(map[string]any{
				"flavors": []map[string]any{{"id": "other", "name": "other-flavor"}},
			})

		case r.Method == http.MethodPost && r.URL.Path == "/ports":
			body := serverBody(t, r)
			port := body["port"].(map[string]any)

			th.AssertEquals(t, "machine-req", port["name"])
			th.AssertEquals(t, "net-uuid", port["network_id"])

			if _, ok := port["security_groups"]; ok {
				t.Error("port create must not carry security_groups when none are set")
			}

			w.WriteHeader(http.StatusCreated)
			_ = json.NewEncoder(w).Encode(map[string]any{"port": map[string]any{"id": "port-main", "name": "machine-req"}})

		case r.Method == http.MethodPost && r.URL.Path == "/servers":
			body := serverBody(t, r)
			server := body["server"].(map[string]any)

			if _, ok := body["os:scheduler_hints"]; ok {
				t.Error("server create must not carry os:scheduler_hints when no server group is set")
			}

			if _, ok := server["availability_zone"]; ok {
				t.Error("server create must not carry availability_zone when not set")
			}

			// user_data is always on the wire (base64 of empty stays
			// empty) — pin the actual behavior, not the assumption.
			th.AssertEquals(t, "", server["user_data"])

			th.AssertEquals(t, false, server["config_drive"])

			bdm, ok := server["block_device_mapping_v2"].([]any)
			if !ok || len(bdm) != 1 {
				t.Fatalf("unexpected block device mapping %v", server["block_device_mapping_v2"])
			}

			boot, _ := bdm[0].(map[string]any)
			th.AssertEquals(t, "image", boot["source_type"])
			th.AssertEquals(t, "img-uuid", boot["uuid"])
			th.AssertEquals(t, float64(10), boot["volume_size"])

			nets := server["networks"].([]any)
			net0, _ := nets[0].(map[string]any)
			th.AssertEquals(t, "port-main", net0["port"])

			w.WriteHeader(http.StatusAccepted)
			_ = json.NewEncoder(w).Encode(map[string]any{"server": map[string]any{"id": "srv-uuid", "status": "BUILD"}})

		default:
			t.Errorf("unexpected request %s %s", r.Method, r.URL)
		}
	}))

	serverID, err := client.CreateServer(context.Background(), zap.NewNop(), ServerRequest{
		Name:        "machine-req",
		ImageID:     "img-uuid",
		Flavor:      "flav-uuid",
		Network:     "net-uuid",
		DiskSizeGB:  10,
		UserData:    "",
		ConfigDrive: false,
	})
	th.AssertNoErr(t, err)
	th.AssertEquals(t, "srv-uuid", serverID)
}

func TestCreateServer_NICPortFails(t *testing.T) {
	deleted := newDeleteTracker()

	client := newTestClient(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")

		switch {
		case r.Method == http.MethodGet && r.URL.Path == "/networks":
			switch r.URL.Query().Get("name") {
			case "tenant":
				_ = json.NewEncoder(w).Encode(map[string]any{"networks": []map[string]any{{"id": "net-uuid", "name": "tenant"}}})
			case "storage-net":
				_ = json.NewEncoder(w).Encode(map[string]any{"networks": []map[string]any{{"id": "net2-uuid", "name": "storage-net"}}})
			default:
				t.Errorf("unexpected network lookup %q", r.URL.RawQuery)
			}

		case r.Method == http.MethodPost && r.URL.Path == "/ports":
			body := serverBody(t, r)
			port := body["port"].(map[string]any)

			if port["network_id"] == "net2-uuid" {
				w.WriteHeader(http.StatusInternalServerError)
				_ = json.NewEncoder(w).Encode(map[string]any{"error": map[string]any{"message": "boom"}})

				return
			}

			w.WriteHeader(http.StatusCreated)
			_ = json.NewEncoder(w).Encode(map[string]any{"port": map[string]any{"id": "port-main", "name": "machine-req"}})

		case r.Method == http.MethodDelete && strings.HasPrefix(r.URL.Path, "/ports/"):
			deleted.record(strings.TrimPrefix(r.URL.Path, "/ports/"))
			w.WriteHeader(http.StatusNoContent)

		default:
			t.Errorf("unexpected request %s %s", r.Method, r.URL)
		}
	}))

	_, err := client.CreateServer(context.Background(), zap.NewNop(), ServerRequest{
		Name:           "machine-req",
		ImageID:        "img-uuid",
		Flavor:         "flav-uuid",
		Network:        "tenant",
		DiskSizeGB:     10,
		AdditionalNICs: []AdditionalNIC{{Network: "storage-net"}},
	})
	if err == nil {
		t.Fatal("expected error, got nil")
	}

	if !strings.Contains(err.Error(), "creating additional nic port") {
		t.Fatalf("expected nic port error, got %v", err)
	}

	deleted.assertDeleted(t, "port-main")
}

func TestCreateServer_ServerCreateFails(t *testing.T) {
	deleted := newDeleteTracker()

	client := newTestClient(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")

		switch {
		case r.Method == http.MethodGet && r.URL.Path == "/networks":
			switch r.URL.Query().Get("name") {
			case "tenant":
				_ = json.NewEncoder(w).Encode(map[string]any{"networks": []map[string]any{{"id": "net-uuid", "name": "tenant"}}})
			case "storage-net":
				_ = json.NewEncoder(w).Encode(map[string]any{"networks": []map[string]any{{"id": "net2-uuid", "name": "storage-net"}}})
			default:
				t.Errorf("unexpected network lookup %q", r.URL.RawQuery)
			}

		case r.Method == http.MethodPost && r.URL.Path == "/ports":
			body := serverBody(t, r)
			port := body["port"].(map[string]any)

			var portID string
			switch port["network_id"] {
			case "net-uuid":
				portID = "port-main"
			case "net2-uuid":
				portID = "port-nic-0"
			default:
				t.Errorf("unexpected port create for network %v", port["network_id"])

				return
			}

			w.WriteHeader(http.StatusCreated)
			_ = json.NewEncoder(w).Encode(map[string]any{"port": map[string]any{"id": portID, "name": "machine-req"}})

		case r.Method == http.MethodGet && r.URL.Path == "/flavors/detail":
			_ = json.NewEncoder(w).Encode(map[string]any{
				"flavors": []map[string]any{{"id": "flav-uuid", "name": "m1.large", "vcpus": 2, "ram": 4096, "disk": 20}},
			})

		case r.Method == http.MethodPost && r.URL.Path == "/servers":
			w.WriteHeader(http.StatusInternalServerError)
			_ = json.NewEncoder(w).Encode(map[string]any{"computeFault": map[string]any{"message": "boom", "code": 500}})

		case r.Method == http.MethodDelete && strings.HasPrefix(r.URL.Path, "/ports/"):
			deleted.record(strings.TrimPrefix(r.URL.Path, "/ports/"))
			w.WriteHeader(http.StatusNoContent)

		default:
			t.Errorf("unexpected request %s %s", r.Method, r.URL)
		}
	}))

	_, err := client.CreateServer(context.Background(), zap.NewNop(), ServerRequest{
		Name:           "machine-req",
		ImageID:        "img-uuid",
		Flavor:         "m1.large",
		Network:        "tenant",
		DiskSizeGB:     10,
		AdditionalNICs: []AdditionalNIC{{Network: "storage-net"}},
	})
	if err == nil {
		t.Fatal("expected error, got nil")
	}

	if !strings.Contains(err.Error(), "creating nova server") {
		t.Fatalf("expected server create error, got %v", err)
	}

	deleted.assertDeleted(t, "port-main")
	deleted.assertDeleted(t, "port-nic-0")
}

func TestCreateServer_ServerGroupTypo(t *testing.T) {
	deleted := newDeleteTracker()

	client := newTestClient(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")

		switch {
		case r.Method == http.MethodGet && r.URL.Path == "/networks":
			_ = json.NewEncoder(w).Encode(map[string]any{"networks": []map[string]any{{"id": "net-uuid", "name": "tenant"}}})

		case r.Method == http.MethodPost && r.URL.Path == "/ports":
			w.WriteHeader(http.StatusCreated)
			_ = json.NewEncoder(w).Encode(map[string]any{"port": map[string]any{"id": "port-main", "name": "machine-req"}})

		case r.Method == http.MethodGet && r.URL.Path == "/flavors/detail":
			_ = json.NewEncoder(w).Encode(map[string]any{
				"flavors": []map[string]any{{"id": "flav-uuid", "name": "m1.large", "vcpus": 2, "ram": 4096, "disk": 20}},
			})

		case r.Method == http.MethodGet && r.URL.Path == "/os-server-groups":
			_ = json.NewEncoder(w).Encode(map[string]any{
				"server_groups": []map[string]any{{"id": "123e4567-e89b-12d3-a456-426614174001", "name": "anti-affinity", "policies": []string{"anti-affinity"}}},
			})

		case r.Method == http.MethodDelete && strings.HasPrefix(r.URL.Path, "/ports/"):
			deleted.record(strings.TrimPrefix(r.URL.Path, "/ports/"))
			w.WriteHeader(http.StatusNoContent)

		default:
			t.Errorf("unexpected request %s %s", r.Method, r.URL)
		}
	}))

	_, err := client.CreateServer(context.Background(), zap.NewNop(), ServerRequest{
		Name:        "machine-req",
		ImageID:     "img-uuid",
		Flavor:      "m1.large",
		Network:     "tenant",
		DiskSizeGB:  10,
		ServerGroup: "typo-name",
	})
	if err == nil {
		t.Fatal("expected error, got nil")
	}

	if !strings.Contains(err.Error(), "not found") {
		t.Fatalf("expected not-found error, got %v", err)
	}

	deleted.assertDeleted(t, "port-main")
}

func TestCreateServer_AmbiguousNetwork(t *testing.T) {
	client := newTestClient(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")

		switch {
		case r.Method == http.MethodGet && r.URL.Path == "/networks":
			_ = json.NewEncoder(w).Encode(map[string]any{"networks": []map[string]any{
				{"id": "net-1", "name": "tenant"},
				{"id": "net-2", "name": "tenant"},
			}})

		default:
			// The network resolution fails before any port is created —
			// anything else (POST /ports, flavors, servers) is a bug.
			t.Errorf("unexpected request %s %s", r.Method, r.URL)
		}
	}))

	_, err := client.CreateServer(context.Background(), zap.NewNop(), ServerRequest{
		Name:       "machine-req",
		ImageID:    "img-uuid",
		Flavor:     "flav-uuid",
		Network:    "tenant",
		DiskSizeGB: 10,
	})
	if err == nil {
		t.Fatal("expected error, got nil")
	}

	if !strings.Contains(err.Error(), "ambiguous") {
		t.Fatalf("expected ambiguity error, got %v", err)
	}
}

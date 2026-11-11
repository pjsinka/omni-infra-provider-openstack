// This Source Code Form is subject to the terms of the Mozilla Public
// License, v. 2.0. If a copy of the MPL was not distributed with this
// file, You can obtain one at http://mozilla.org/MPL/2.0/.

package openstack

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"strings"
	"testing"

	th "github.com/gophercloud/gophercloud/testhelper"
	"go.uber.org/zap"
)

func TestDeleteServer_PendingFirstRound(t *testing.T) {
	client := newTestClient(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")

		switch {
		case r.Method == http.MethodDelete && r.URL.Path == "/servers/srv-uuid":
			w.WriteHeader(http.StatusNoContent)

		default:
			// The first round returns early — no port listing may happen.
			t.Errorf("unexpected request %s %s", r.Method, r.URL)
		}
	}))

	err := client.DeleteServer(context.Background(), zap.NewNop(), "srv-uuid", "machine-req")
	if !errors.Is(err, ErrCleanupPending) {
		t.Fatalf("expected ErrCleanupPending, got %v", err)
	}
}

func TestDeleteServer_ServerGoneAllClean(t *testing.T) {
	// The leftover scan is filtered by name, the NIC scan lists
	// everything. Both are GET /ports — route on the name parameter.
	client := newTestClient(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")

		switch {
		case r.Method == http.MethodDelete && r.URL.Path == "/servers/srv-uuid":
			w.WriteHeader(http.StatusNotFound)
			_ = json.NewEncoder(w).Encode(map[string]any{"itemNotFound": map[string]any{"message": "not found", "code": 404}})

		case r.Method == http.MethodGet && r.URL.Path == "/ports" && r.URL.Query().Get("name") == "machine-req":
			_ = json.NewEncoder(w).Encode(map[string]any{"ports": []map[string]any{}})

		case r.Method == http.MethodGet && r.URL.Path == "/ports":
			_ = json.NewEncoder(w).Encode(map[string]any{"ports": []map[string]any{}})

		default:
			t.Errorf("unexpected request %s %s", r.Method, r.URL)
		}
	}))

	th.AssertNoErr(t, client.DeleteServer(context.Background(), zap.NewNop(), "srv-uuid", "machine-req"))
}

func TestDeleteServer_LeftoverPortDeleted(t *testing.T) {
	deleted := newDeleteTracker()

	client := newTestClient(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")

		switch {
		case r.Method == http.MethodDelete && r.URL.Path == "/servers/srv-uuid":
			w.WriteHeader(http.StatusNotFound)
			_ = json.NewEncoder(w).Encode(map[string]any{"itemNotFound": map[string]any{"message": "not found", "code": 404}})

		case r.Method == http.MethodGet && r.URL.Path == "/ports" && r.URL.Query().Get("name") == "machine-req":
			_ = json.NewEncoder(w).Encode(map[string]any{"ports": []map[string]any{
				{"id": "port-main", "name": "machine-req"},
			}})

		case r.Method == http.MethodGet && r.URL.Path == "/ports":
			_ = json.NewEncoder(w).Encode(map[string]any{"ports": []map[string]any{}})

		case r.Method == http.MethodDelete && r.URL.Path == "/ports/port-main":
			deleted.record("port-main")
			w.WriteHeader(http.StatusNoContent)

		default:
			t.Errorf("unexpected request %s %s", r.Method, r.URL)
		}
	}))

	// A found (and deleted) port still counts as not-done: cleanup is only
	// confirmed by absence on the NEXT round — the caller retries.
	err := client.DeleteServer(context.Background(), zap.NewNop(), "srv-uuid", "machine-req")
	if !errors.Is(err, ErrCleanupPending) {
		t.Fatalf("expected ErrCleanupPending, got %v", err)
	}

	deleted.assertDeleted(t, "port-main")
}

func TestDeleteServer_PortDelete409(t *testing.T) {
	client := newTestClient(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")

		switch {
		case r.Method == http.MethodDelete && r.URL.Path == "/servers/srv-uuid":
			w.WriteHeader(http.StatusNotFound)
			_ = json.NewEncoder(w).Encode(map[string]any{"itemNotFound": map[string]any{"message": "not found", "code": 404}})

		case r.Method == http.MethodGet && r.URL.Path == "/ports" && r.URL.Query().Get("name") == "machine-req":
			_ = json.NewEncoder(w).Encode(map[string]any{"ports": []map[string]any{
				{"id": "port-main", "name": "machine-req"},
			}})

		case r.Method == http.MethodDelete && r.URL.Path == "/ports/port-main":
			// Port still attached to the server (Nova detaches
			// asynchronously) — Neutron refuses the delete.
			w.WriteHeader(http.StatusConflict)
			_ = json.NewEncoder(w).Encode(map[string]any{"conflict": map[string]any{"message": "port in use", "code": 409}})

		default:
			t.Errorf("unexpected request %s %s", r.Method, r.URL)
		}
	}))

	err := client.DeleteServer(context.Background(), zap.NewNop(), "srv-uuid", "machine-req")
	if err == nil {
		t.Fatal("expected error, got nil")
	}

	if errors.Is(err, ErrCleanupPending) {
		t.Fatalf("expected hard error, got ErrCleanupPending: %v", err)
	}

	if !strings.Contains(err.Error(), "deleting leftover port") {
		t.Fatalf("expected leftover port error, got %v", err)
	}
}

func TestDeleteServer_NICPrefixCleanup(t *testing.T) {
	deleted := newDeleteTracker()

	client := newTestClient(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")

		switch {
		case r.Method == http.MethodDelete && r.URL.Path == "/servers/srv-uuid":
			w.WriteHeader(http.StatusNotFound)
			_ = json.NewEncoder(w).Encode(map[string]any{"itemNotFound": map[string]any{"message": "not found", "code": 404}})

		case r.Method == http.MethodGet && r.URL.Path == "/ports" && r.URL.Query().Get("name") == "machine-req":
			_ = json.NewEncoder(w).Encode(map[string]any{"ports": []map[string]any{}})

		case r.Method == http.MethodGet && r.URL.Path == "/ports":
			_ = json.NewEncoder(w).Encode(map[string]any{"ports": []map[string]any{
				{"id": "nic-1", "name": "machine-req-nic-0"},
				{"id": "nic-2", "name": "other-machine-nic-0"},
			}})

		case r.Method == http.MethodDelete && r.URL.Path == "/ports/nic-1":
			// Still attached — blocked, retry later.
			w.WriteHeader(http.StatusConflict)
			_ = json.NewEncoder(w).Encode(map[string]any{"conflict": map[string]any{"message": "port in use", "code": 409}})

		case r.Method == http.MethodDelete && r.URL.Path == "/ports/nic-2":
			deleted.record("nic-2")
			w.WriteHeader(http.StatusNoContent)

		default:
			t.Errorf("unexpected request %s %s", r.Method, r.URL)
		}
	}))

	err := client.DeleteServer(context.Background(), zap.NewNop(), "srv-uuid", "machine-req")
	if !errors.Is(err, ErrCleanupPending) {
		t.Fatalf("expected ErrCleanupPending, got %v", err)
	}

	deleted.assertNotDeleted(t, "nic-2")
}

func TestDeleteServer_ServerDeleteError(t *testing.T) {
	client := newTestClient(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")

		switch {
		case r.Method == http.MethodDelete && r.URL.Path == "/servers/srv-uuid":
			w.WriteHeader(http.StatusInternalServerError)
			_ = json.NewEncoder(w).Encode(map[string]any{"computeFault": map[string]any{"message": "boom", "code": 500}})

		default:
			t.Errorf("unexpected request %s %s", r.Method, r.URL)
		}
	}))

	err := client.DeleteServer(context.Background(), zap.NewNop(), "srv-uuid", "machine-req")
	if err == nil {
		t.Fatal("expected error, got nil")
	}

	if errors.Is(err, ErrCleanupPending) {
		t.Fatalf("expected hard error, got ErrCleanupPending: %v", err)
	}

	if !strings.Contains(err.Error(), "deleting server") {
		t.Fatalf("expected deleting server error, got %v", err)
	}
}

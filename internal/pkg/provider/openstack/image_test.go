// This Source Code Form is subject to the terms of the Mozilla Public
// License, v. 2.0. If a copy of the MPL was not distributed with this
// file, You can obtain one at http://mozilla.org/MPL/2.0/.

package openstack

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	th "github.com/gophercloud/gophercloud/testhelper"
	"go.uber.org/zap"
)

func TestEnsureImage_AlreadyActive(t *testing.T) {
	client := newTestClient(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")

		switch {
		case r.Method == http.MethodGet && r.URL.Path == "/images":
			th.AssertEquals(t, "key", r.URL.Query().Get("name"))

			_ = json.NewEncoder(w).Encode(map[string]any{"images": []map[string]any{
				{"id": "img-uuid", "name": "key", "status": "active"},
			}})

		default:
			t.Errorf("unexpected request %s %s", r.Method, r.URL)
		}
	}))

	// The factory URL is never fetched — an invalid URL would fail the test.
	id, err := client.EnsureImage(context.Background(), zap.NewNop(), "key", "http://factory.invalid/image", nil, nil)
	th.AssertNoErr(t, err)
	th.AssertEquals(t, "img-uuid", id)
}

func TestEnsureImage_StillSaving(t *testing.T) {
	client := newTestClient(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")

		switch {
		case r.Method == http.MethodGet && r.URL.Path == "/images":
			_ = json.NewEncoder(w).Encode(map[string]any{"images": []map[string]any{
				{"id": "img-uuid", "name": "key", "status": "saving"},
			}})

		default:
			// Another machine is uploading the same image: the caller
			// must retry, no create attempt may happen.
			t.Errorf("unexpected request %s %s", r.Method, r.URL)
		}
	}))

	_, err := client.EnsureImage(context.Background(), zap.NewNop(), "key", "http://factory.invalid/image", nil, nil)
	if err == nil {
		t.Fatal("expected waiting error, got nil")
	}

	if !strings.Contains(err.Error(), "waiting for it to become active") {
		t.Fatalf("expected waiting error, got %v", err)
	}
}

func TestEnsureImage_Uploads(t *testing.T) {
	factory := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		th.AssertEquals(t, "token123", r.Header.Get("X-Factory-Token"))

		w.Header().Set("Content-Type", "application/octet-stream")
		_, _ = w.Write([]byte("fake-qcow2"))
	}))
	t.Cleanup(factory.Close)

	client := newTestClient(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")

		switch {
		case r.Method == http.MethodGet && r.URL.Path == "/images":
			_ = json.NewEncoder(w).Encode(map[string]any{"images": []map[string]any{}})

		case r.Method == http.MethodPost && r.URL.Path == "/images":
			body := serverBody(t, r)
			th.AssertEquals(t, "key", body["name"])
			th.AssertEquals(t, "qcow2", body["disk_format"])
			th.AssertEquals(t, "bare", body["container_format"])

			// Glance v2 returns the image object directly — no envelope.
			w.WriteHeader(http.StatusCreated)
			_ = json.NewEncoder(w).Encode(map[string]any{"id": "img-uuid", "name": "key", "status": "queued"})

		case r.Method == http.MethodPut && r.URL.Path == "/images/img-uuid/file":
			th.AssertEquals(t, "application/octet-stream", r.Header.Get("Content-Type"))

			data, readErr := io.ReadAll(r.Body)
			th.AssertNoErr(t, readErr)
			th.AssertEquals(t, "fake-qcow2", string(data))

			w.WriteHeader(http.StatusNoContent)

		default:
			t.Errorf("unexpected request %s %s", r.Method, r.URL)
		}
	}))

	id, err := client.EnsureImage(context.Background(), zap.NewNop(), "key", factory.URL+"/image", http.Header{"X-Factory-Token": []string{"token123"}}, nil)
	th.AssertNoErr(t, err)
	th.AssertEquals(t, "img-uuid", id)
}

func TestEnsureImage_FactoryNon200(t *testing.T) {
	factory := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusNotFound)
	}))
	t.Cleanup(factory.Close)

	deleted := newDeleteTracker()

	client := newTestClient(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")

		switch {
		case r.Method == http.MethodGet && r.URL.Path == "/images":
			_ = json.NewEncoder(w).Encode(map[string]any{"images": []map[string]any{}})

		case r.Method == http.MethodPost && r.URL.Path == "/images":
			w.WriteHeader(http.StatusCreated)
			_ = json.NewEncoder(w).Encode(map[string]any{"id": "img-uuid", "name": "key", "status": "queued"})

		case r.Method == http.MethodDelete && r.URL.Path == "/images/img-uuid":
			deleted.record("img-uuid")
			w.WriteHeader(http.StatusNoContent)

		default:
			t.Errorf("unexpected request %s %s", r.Method, r.URL)
		}
	}))

	_, err := client.EnsureImage(context.Background(), zap.NewNop(), "key", factory.URL+"/image", nil, nil)
	if err == nil {
		t.Fatal("expected factory error, got nil")
	}

	if !strings.Contains(err.Error(), "unexpected status 404") {
		t.Fatalf("expected factory status error, got %v", err)
	}

	deleted.assertDeleted(t, "img-uuid")
}

func TestEnsureImage_UploadFails(t *testing.T) {
	factory := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/octet-stream")
		_, _ = w.Write([]byte("fake-qcow2"))
	}))
	t.Cleanup(factory.Close)

	deleted := newDeleteTracker()

	client := newTestClient(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")

		switch {
		case r.Method == http.MethodGet && r.URL.Path == "/images":
			_ = json.NewEncoder(w).Encode(map[string]any{"images": []map[string]any{}})

		case r.Method == http.MethodPost && r.URL.Path == "/images":
			w.WriteHeader(http.StatusCreated)
			_ = json.NewEncoder(w).Encode(map[string]any{"id": "img-uuid", "name": "key", "status": "queued"})

		case r.Method == http.MethodPut && r.URL.Path == "/images/img-uuid/file":
			w.WriteHeader(http.StatusInternalServerError)
			_ = json.NewEncoder(w).Encode(map[string]any{"error": map[string]any{"message": "boom"}})

		case r.Method == http.MethodDelete && r.URL.Path == "/images/img-uuid":
			deleted.record("img-uuid")
			w.WriteHeader(http.StatusNoContent)

		default:
			t.Errorf("unexpected request %s %s", r.Method, r.URL)
		}
	}))

	_, err := client.EnsureImage(context.Background(), zap.NewNop(), "key", factory.URL+"/image", nil, nil)
	if err == nil {
		t.Fatal("expected upload error, got nil")
	}

	if !strings.Contains(err.Error(), "uploading image data to glance") {
		t.Fatalf("expected upload error, got %v", err)
	}

	deleted.assertDeleted(t, "img-uuid")
}

func TestEnsureImage_SetsFirmwareProperty(t *testing.T) {
	for _, firmware := range []string{"uefi", "bios"} {
		t.Run(firmware, func(t *testing.T) {
			factory := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				w.Header().Set("Content-Type", "application/octet-stream")
				_, _ = w.Write([]byte("fake-qcow2"))
			}))
			t.Cleanup(factory.Close)

			client := newTestClient(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				w.Header().Set("Content-Type", "application/json")

				switch {
				case r.Method == http.MethodGet && r.URL.Path == "/images":
					_ = json.NewEncoder(w).Encode(map[string]any{"images": []map[string]any{}})

				case r.Method == http.MethodPost && r.URL.Path == "/images":
					body := serverBody(t, r)
					th.AssertEquals(t, "key-"+firmware, body["name"])
					th.AssertEquals(t, firmware, body["hw_firmware_type"])

					w.WriteHeader(http.StatusCreated)
					_ = json.NewEncoder(w).Encode(map[string]any{"id": "img-uuid", "name": "key", "status": "queued"})

				case r.Method == http.MethodPut && r.URL.Path == "/images/img-uuid/file":
					w.WriteHeader(http.StatusNoContent)

				default:
					t.Errorf("unexpected request %s %s", r.Method, r.URL)
				}
			}))

			id, err := client.EnsureImage(context.Background(), zap.NewNop(),
				"key-"+firmware, factory.URL+"/image", nil,
				map[string]string{"hw_firmware_type": firmware})
			th.AssertNoErr(t, err)
			th.AssertEquals(t, "img-uuid", id)
		})
	}
}

func TestEnsureImage_NoPropertiesOmittedFromBody(t *testing.T) {
	factory := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/octet-stream")
		_, _ = w.Write([]byte("fake-qcow2"))
	}))
	t.Cleanup(factory.Close)

	client := newTestClient(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")

		switch {
		case r.Method == http.MethodGet && r.URL.Path == "/images":
			_ = json.NewEncoder(w).Encode(map[string]any{"images": []map[string]any{}})

		case r.Method == http.MethodPost && r.URL.Path == "/images":
			body := serverBody(t, r)
			if _, ok := body["hw_firmware_type"]; ok {
				t.Errorf("hw_firmware_type must be omitted for nil properties, got %v", body["hw_firmware_type"])
			}

			w.WriteHeader(http.StatusCreated)
			_ = json.NewEncoder(w).Encode(map[string]any{"id": "img-uuid", "name": "key", "status": "queued"})

		case r.Method == http.MethodPut && r.URL.Path == "/images/img-uuid/file":
			w.WriteHeader(http.StatusNoContent)

		default:
			t.Errorf("unexpected request %s %s", r.Method, r.URL)
		}
	}))

	id, err := client.EnsureImage(context.Background(), zap.NewNop(), "key", factory.URL+"/image", nil, nil)
	th.AssertNoErr(t, err)
	th.AssertEquals(t, "img-uuid", id)
}

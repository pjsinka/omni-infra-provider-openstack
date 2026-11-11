// This Source Code Form is subject to the terms of the Mozilla Public
// License, v. 2.0. If a copy of the MPL was not distributed with this
// file, You can obtain one at http://mozilla.org/MPL/2.0/.

package openstack

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/gophercloud/gophercloud"
)

// newTestClient mirrors New(): one provider token, one endpoint per
// service, compute microversion 2.67. All service clients point at the
// same test server; Nova, Neutron and Glance paths are distinct.
func newTestClient(t *testing.T, handler http.Handler) *Client {
	t.Helper()

	server := httptest.NewServer(handler)
	t.Cleanup(server.Close)

	providerClient := &gophercloud.ProviderClient{TokenID: "test-token"}

	base := &gophercloud.ServiceClient{
		ProviderClient: providerClient,
		Endpoint:      server.URL + "/",
	}

	compute := *base
	compute.Microversion = "2.67"

	return &Client{
		Cloud:   "test",
		image:   base,
		compute: &compute,
		network: base,
		volume:  base,
	}
}

// serverBody decodes a POST /servers request body and returns the inner
// "server" object.
func serverBody(t *testing.T, r *http.Request) map[string]any {
	t.Helper()

	var body map[string]any

	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		t.Fatalf("decoding server create body: %v", err)
	}

	server, ok := body["server"].(map[string]any)
	if !ok {
		t.Fatalf("no server object in create body: %v", body)
	}

	return server
}

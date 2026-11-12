// This Source Code Form is subject to the terms of the Mozilla Public
// License, v. 2.0. If a copy of the MPL was not distributed with this
// file, You can obtain one at http://mozilla.org/MPL/2.0/.

//go:build e2e

// Package e2e contains the Tier-2 end-to-end test: provision a machine
// through the OpenStack infra provider (Omni MachineSet -> MachineRequest ->
// Nova server) and deprovision it again, asserting the OpenStack side at
// every step. It needs a running Omni instance with the provider registered
// and connected, plus a reachable OpenStack cloud; without the environment
// variables below the test skips.
//
// Environment:
//
//	OMNI_ENDPOINT             e.g. https://localhost:8080
//	OMNI_SERVICE_ACCOUNT_KEY  bootstrap admin service account key (Admin role
//	                          is required to create MachineClasses/MachineSets)
//	OS_CLOUD                  cloud name in clouds.yaml
//	OS_CLIENT_CONFIG_FILE     optional; clouds.yaml path (gophercloud
//	                          clientconfig search paths apply otherwise)
//	OMNICTL_BIN               optional omnictl path (defaults to omnictl on PATH)
package e2e

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"strings"
	"testing"
	"time"

	"github.com/cosi-project/runtime/pkg/safe"
	"github.com/cosi-project/runtime/pkg/state"
	"github.com/gophercloud/gophercloud"
	"github.com/gophercloud/gophercloud/openstack/blockstorage/v3/volumes"
	"github.com/gophercloud/gophercloud/openstack/compute/v2/servers"
	"github.com/gophercloud/gophercloud/openstack/networking/v2/extensions/security/groups"
	"github.com/gophercloud/gophercloud/openstack/networking/v2/networks"
	"github.com/gophercloud/gophercloud/openstack/networking/v2/ports"
	"github.com/gophercloud/utils/openstack/clientconfig"

	"github.com/siderolabs/omni/client/api/omni/specs"
	omniclient "github.com/siderolabs/omni/client/pkg/client"
	"github.com/siderolabs/omni/client/pkg/omni/resources/infra"
	"github.com/siderolabs/omni/client/pkg/omni/resources/omni"

	"github.com/pjsinka/omni-infra-provider-openstack/internal/pkg/provider/openstack"
)

// TestE2EProvisionDeprovision applies a suffixed Cluster/MachineClass/
// MachineSet to Omni, waits for the provider to provision the Nova server,
// asserts the OpenStack resources, deprovisions the machine set and waits
// for the OpenStack side to return to the baseline.
//
// The resource names carry a random suffix so leftovers from a failed run
// never collide, and a single t.Cleanup purges anything prefixed omni-e2e.
func TestE2EProvisionDeprovision(t *testing.T) {
	omniEndpoint := os.Getenv("OMNI_ENDPOINT")
	saKey := os.Getenv("OMNI_SERVICE_ACCOUNT_KEY")
	cloud := os.Getenv("OS_CLOUD")
	if omniEndpoint == "" || saKey == "" || cloud == "" {
		t.Skip("e2e env not set: OMNI_ENDPOINT / OMNI_SERVICE_ACCOUNT_KEY / OS_CLOUD")
	}

	ctx := context.Background()

	// Omni client: version discovery + MachineRequestStatus polling.
	omniC, err := omniclient.New(omniEndpoint,
		omniclient.WithServiceAccount(saKey),
		omniclient.WithInsecureSkipTLSVerify(true),
	)
	if err != nil {
		t.Fatalf("creating omni client: %v", err)
	}

	t.Cleanup(func() {
		if err := omniC.Close(); err != nil {
			t.Logf("closing omni client: %v", err)
		}
	})

	// OpenStack clients: same construction as the provider (clientconfig for
	// auth/compute/network; the exported NewBlockStorageClient resolves
	// Cinder's official "block-storage" catalog type with legacy fallback).
	cloudOpts := &clientconfig.ClientOpts{Cloud: cloud}

	pc, err := clientconfig.AuthenticatedClient(cloudOpts)
	if err != nil {
		t.Fatalf("openstack authentication: %v", err)
	}

	computeClient, err := clientconfig.NewServiceClient("compute", cloudOpts)
	if err != nil {
		t.Fatalf("creating compute client: %v", err)
	}

	networkClient, err := clientconfig.NewServiceClient("network", cloudOpts)
	if err != nil {
		t.Fatalf("creating network client: %v", err)
	}

	cloudCfg, err := clientconfig.GetCloudFromYAML(cloudOpts)
	if err != nil {
		t.Fatalf("reading cloud config: %v", err)
	}

	volClient, err := openstack.NewBlockStorageClient(pc, gophercloud.EndpointOpts{
		Region:       cloudCfg.RegionName,
		Availability: gophercloud.Availability(cloudCfg.EndpointType),
	})
	if err != nil {
		t.Fatalf("creating volume client: %v", err)
	}

	// Suffix: isolates this run's resources from leftovers of failed runs.
	suffix, err := randomSuffix()
	if err != nil {
		t.Fatalf("generating suffix: %v", err)
	}

	clusterName := "omni-e2e-" + suffix
	mcName := "omni-e2e-mc-" + suffix
	msName := clusterName + "-control-planes"
	sgName := "e2e-sg-" + suffix
	t.Logf("suffix %s: cluster=%s machineclass=%s machineset=%s sg=%s", suffix, clusterName, mcName, msName, sgName)

	// Baseline resource IDs before anything is created: the deprovision wait
	// compares counts against them, and the volume purge only ever deletes
	// volumes that are NOT in the baseline (i.e. created during this run).
	baseSrv, baseVol, basePort := baselineIDs(t, computeClient, networkClient, volClient)

	// Security group: idempotent. No extra rules — Neutron 2026.2 creates
	// allow-all egress rules (IPv4+IPv6) together with the group; an extra
	// egress rule 409-conflicted in Run 15. Machines reach Omni via those
	// defaults.
	sgID := ensureSecurityGroup(t, networkClient, sgName)

	// One cleanup: best-effort machineset delete, prefix purge, volume
	// purge, security group delete, Omni resource delete. It no-ops after
	// the happy path and minimizes zombies after any failure.
	t.Cleanup(func() {
		t.Log("cleanup: purging leftovers")

		omnictlBestEffort(t, "delete", "machineSets.omni.sidero.dev", msName)
		purgeServers(t, computeClient)
		purgeVolumes(t, volClient, baseVol)
		purgePorts(t, networkClient)
		deleteSecurityGroupBestEffort(t, networkClient, sgID, sgName)
		omnictlBestEffort(t, "delete", "machineclasses.omni.sidero.dev", mcName)
		omnictlBestEffort(t, "delete", "clusters.omni.sidero.dev", clusterName)
	})

	// Versions from the Omni instance itself — never hardcode a pair its
	// image factory may not support.
	talosVersion, kubernetesVersion, err := selectOmniVersions(ctx, omniC.Omni().State())
	if err != nil {
		t.Fatalf("selecting versions: %v", err)
	}

	t.Logf("selected: talos=%s kubernetes=%s", talosVersion, kubernetesVersion)

	// Render templates: suffix + versions.
	r := nameReplacer(clusterName, mcName, msName, sgName, talosVersion, kubernetesVersion)
	clusterYAML := writeTemp(t, "cluster.yaml", renderTemplate(t, "cluster.template.yaml", r))
	mcYAML := writeTemp(t, "machineclass.yaml", renderTemplate(t, "machineclass.yaml", r))
	msYAML := writeTemp(t, "machineset.yaml", renderTemplate(t, "machineset.yaml", r))

	// Apply: cluster and machineclass before the machineset (Omni validates
	// the cluster reference and the machine allocation name).
	omnictl(t, "apply", "-f", clusterYAML)
	omnictl(t, "apply", "-f", mcYAML)
	omnictl(t, "apply", "-f", msYAML)

	// MachineRequest completion: the provider writes stage/status; PROVISIONED
	// implies the Nova server is ACTIVE (the provider sets the stage only
	// after the server is up — proven in Run 22).
	reqID := pollMachineRequest(t, ctx, omniC.Omni().State(), msName)

	// Nova asserts.
	srvList, err := listServers(computeClient)
	if err != nil {
		t.Fatalf("listing servers: %v", err)
	}

	var srv servers.Server

	for _, s := range srvList {
		if s.Name == reqID {
			srv = s

			break
		}
	}

	if srv.ID == "" {
		t.Fatalf("no nova server named %s (request id)", reqID)
	}

	if srv.Status != "ACTIVE" {
		t.Errorf("server status = %s, want ACTIVE", srv.Status)
	}

	if got := fmt.Sprintf("%v", srv.Flavor["original_name"]); got != "m1.small" {
		t.Errorf("server flavor = %s, want m1.small", got)
	}

	if got := srv.Metadata["managed-by"]; got != "omni-infra-provider-openstack" {
		t.Errorf("server metadata managed-by = %q, want omni-infra-provider-openstack", got)
	}

	// Boot volume: attached to the server, 10 GB, in use.
	volList, err := listVolumes(volClient)
	if err != nil {
		t.Fatalf("listing volumes: %v", err)
	}

	var bootVol volumes.Volume

	for _, v := range volList {
		for _, att := range v.Attachments {
			if att.ServerID == srv.ID {
				bootVol = v
			}
		}
	}

	if bootVol.ID == "" {
		t.Fatalf("no volume attached to server %s (%s)", srv.ID, reqID)
	}

	if bootVol.Size != 10 {
		t.Errorf("boot volume size = %d, want 10", bootVol.Size)
	}

	if bootVol.Status != "in-use" {
		t.Errorf("boot volume status = %s, want in-use", bootVol.Status)
	}

	// Port: exactly one, in network "private", carrying the test security
	// group (the provider attaches it by name — Run 21 proved the port's
	// security_groups list contains the e2e-sg UUID).
	portList, err := listPorts(networkClient)
	if err != nil {
		t.Fatalf("listing ports: %v", err)
	}

	var machinePort ports.Port

	for _, p := range portList {
		if p.DeviceID == srv.ID {
			machinePort = p

			break
		}
	}

	if machinePort.ID == "" {
		t.Fatalf("no port on server %s", srv.ID)
	}

	netList, err := networks.List(networkClient, networks.ListOpts{Name: "private"}).AllPages()
	if err != nil {
		t.Fatalf("listing networks: %v", err)
	}

	netNames, err := networks.ExtractNetworks(netList)
	if err != nil {
		t.Fatalf("extracting networks: %v", err)
	}

	if len(netNames) != 1 || netNames[0].Name != "private" {
		t.Fatalf("network %q not found or not unique: %+v", "private", netNames)
	}

	if machinePort.NetworkID != netNames[0].ID {
		t.Errorf("port network = %s, want private (%s)", machinePort.NetworkID, netNames[0].ID)
	}

	if !containsString(machinePort.SecurityGroups, sgID) {
		t.Errorf("port security groups = %v, want to contain %s (%s)", machinePort.SecurityGroups, sgID, sgName)
	}

	// Deprovision: delete the machine set, wait for the OpenStack side to
	// return to baseline (server, boot volume — delete_on_termination — and
	// provider-created port all go away).
	omnictl(t, "delete", "machineSets.omni.sidero.dev", msName)

	waitFor(t, 10*time.Minute, 30*time.Second, "openstack cleanup to baseline", func() (bool, string) {
		s, sErr := listServers(computeClient)
		v, vErr := listVolumes(volClient)
		p, pErr := listPorts(networkClient)
		if sErr != nil || vErr != nil || pErr != nil {
			return false, fmt.Sprintf("list error: %v %v %v", sErr, vErr, pErr)
		}

		desc := fmt.Sprintf("servers=%d/%d volumes=%d/%d ports=%d/%d",
			len(s), len(baseSrv), len(v), len(baseVol), len(p), len(basePort))

		return len(s) == len(baseSrv) && len(v) == len(baseVol) && len(p) == len(basePort), desc
	})

	t.Log("E2E COMPLETE: provision + deprovision verified, back to baseline")
}

// omnictlRun runs omnictl (from OMNICTL_BIN or PATH) with the Omni
// environment inherited from the test process. --insecure-skip-tls-verify is
// prepended because the e2e Omni instance serves a self-signed certificate.
func omnictlRun(t *testing.T, args ...string) (string, error) {
	t.Helper()

	bin := os.Getenv("OMNICTL_BIN")
	if bin == "" {
		bin = "omnictl"
	}

	cmd := exec.Command(bin, append([]string{"--insecure-skip-tls-verify"}, args...)...)
	out, err := cmd.CombinedOutput()

	return string(out), err
}

// omnictl runs omnictl and fails the test on any error.
func omnictl(t *testing.T, args ...string) string {
	t.Helper()

	out, err := omnictlRun(t, args...)
	if err != nil {
		t.Fatalf("omnictl %s: %v\n%s", strings.Join(args, " "), err, out)
	}

	return out
}

// omnictlBestEffort runs omnictl and logs instead of failing — for cleanup
// paths where the resource may already be gone.
func omnictlBestEffort(t *testing.T, args ...string) {
	t.Helper()

	out, err := omnictlRun(t, args...)
	if err != nil {
		t.Logf("omnictl %s (best-effort): %v\n%s", strings.Join(args, " "), err, out)
	}
}

// waitFor polls cond every interval until it returns true or the timeout
// expires; the last condition description is included in the failure.
func waitFor(t *testing.T, timeout, interval time.Duration, name string, cond func() (bool, string)) {
	t.Helper()

	deadline := time.Now().Add(timeout)

	var last string

	for time.Now().Before(deadline) {
		ok, desc := cond()
		last = desc
		t.Log(desc)
		if ok {
			return
		}

		time.Sleep(interval)
	}

	t.Fatalf("timed out after %s waiting for %s: %s", timeout, name, last)
}

// pollMachineRequest waits for the MachineRequest Omni created for the
// machine set to reach stage PROVISIONED (which implies the Nova server is
// ACTIVE), returning the request id (== server name == port name). It fails
// the test when the request reaches FAILED or the timeout expires, logging
// the provider's progress (the status field carries provider step names
// like validateRequest/ensureImage/syncMachine).
func pollMachineRequest(t *testing.T, ctx context.Context, st state.State, msName string) string {
	t.Helper()

	deadline := time.Now().Add(20 * time.Minute)

	var (
		reqID    string
		lastDesc string
	)

	for time.Now().Before(deadline) {
		statuses, err := safe.ReaderListAll[*infra.MachineRequestStatus](ctx, st)
		if err != nil {
			lastDesc = "list error: " + err.Error()
		} else {
			for item := range statuses.All() {
				id := item.Metadata().ID()
				if !strings.HasPrefix(id, msName+"-") {
					continue
				}

				v := item.TypedSpec().Value
				desc := fmt.Sprintf("request %s: stage=%s status=%q error=%q", id, v.Stage, v.Status, v.Error)
				if desc != lastDesc {
					t.Log(desc)
					lastDesc = desc
				}

				reqID = id

				switch v.Stage {
				case specs.MachineRequestStatusSpec_PROVISIONED:
					return id
				case specs.MachineRequestStatusSpec_FAILED:
					t.Fatalf("machine request %s failed: %s (last status %q)", id, v.Error, v.Status)
				}
			}

			if reqID == "" && lastDesc == "" {
				lastDesc = "no machine request with prefix " + msName + " yet"
			}
		}

		time.Sleep(30 * time.Second)
	}

	t.Fatalf("machine request for machineset %s did not reach PROVISIONED within 20m: %s", msName, lastDesc)

	return "" // unreachable
}

// selectOmniVersions returns the Talos and Kubernetes version ids to use,
// preferring the maximum stable version the Omni instance reports.
func selectOmniVersions(ctx context.Context, st state.State) (string, string, error) {
	talosList, err := safe.ReaderListAll[*omni.TalosVersion](ctx, st)
	if err != nil {
		return "", "", fmt.Errorf("listing talos versions: %w", err)
	}

	var talosIDs []string

	for item := range talosList.All() {
		talosIDs = append(talosIDs, item.Metadata().ID())
	}

	k8sList, err := safe.ReaderListAll[*omni.KubernetesVersion](ctx, st)
	if err != nil {
		return "", "", fmt.Errorf("listing kubernetes versions: %w", err)
	}

	var k8sIDs []string

	for item := range k8sList.All() {
		k8sIDs = append(k8sIDs, item.Metadata().ID())
	}

	talosVersion, err := selectVersion(talosIDs)
	if err != nil {
		return "", "", fmt.Errorf("talos: %w", err)
	}

	kubernetesVersion, err := selectVersion(k8sIDs)
	if err != nil {
		return "", "", fmt.Errorf("kubernetes: %w", err)
	}

	return talosVersion, kubernetesVersion, nil
}

// ensureSecurityGroup returns the id of the named security group, creating
// it when missing. Idempotent because a leftover from a crashed run with the
// same suffix would make Create fail.
func ensureSecurityGroup(t *testing.T, networkClient *gophercloud.ServiceClient, name string) string {
	t.Helper()

	pages, err := groups.List(networkClient, groups.ListOpts{Name: name}).AllPages()
	if err != nil {
		t.Fatalf("listing security groups: %v", err)
	}

	existing, err := groups.ExtractGroups(pages)
	if err != nil {
		t.Fatalf("extracting security groups: %v", err)
	}

	for _, g := range existing {
		if g.Name == name {
			t.Logf("security group %s exists: %s", name, g.ID)

			return g.ID
		}
	}

	created, err := groups.Create(networkClient, groups.CreateOpts{
		Name:        name,
		Description: "omni e2e machines (egress via default rules)",
	}).Extract()
	if err != nil {
		t.Fatalf("creating security group %s: %v", name, err)
	}

	t.Logf("security group %s created: %s", name, created.ID)

	return created.ID
}

// deleteSecurityGroupBestEffort deletes the security group, retrying while a
// port still references it.
func deleteSecurityGroupBestEffort(t *testing.T, networkClient *gophercloud.ServiceClient, sgID, sgName string) {
	t.Helper()

	for i := range 6 {
		if err := groups.Delete(networkClient, sgID).ExtractErr(); err == nil {
			t.Logf("cleanup: deleted security group %s", sgName)

			return
		} else if i == 5 {
			t.Logf("cleanup: security group %s delete failed: %v (port still attached?)", sgName, err)
		}

		time.Sleep(10 * time.Second)
	}
}

func listServers(computeClient *gophercloud.ServiceClient) ([]servers.Server, error) {
	pages, err := servers.List(computeClient, nil).AllPages()
	if err != nil {
		return nil, err
	}

	return servers.ExtractServers(pages)
}

func listVolumes(volClient *gophercloud.ServiceClient) ([]volumes.Volume, error) {
	pages, err := volumes.List(volClient, nil).AllPages()
	if err != nil {
		return nil, err
	}

	return volumes.ExtractVolumes(pages)
}

func listPorts(networkClient *gophercloud.ServiceClient) ([]ports.Port, error) {
	pages, err := ports.List(networkClient, nil).AllPages()
	if err != nil {
		return nil, err
	}

	return ports.ExtractPorts(pages)
}

// baselineIDs snapshots the ids of all servers, volumes and ports before the
// test creates anything.
func baselineIDs(t *testing.T, computeClient, networkClient, volClient *gophercloud.ServiceClient) (srv, vol, port map[string]struct{}) {
	t.Helper()

	srvList, err := listServers(computeClient)
	if err != nil {
		t.Fatalf("baseline server list: %v", err)
	}

	volList, err := listVolumes(volClient)
	if err != nil {
		t.Fatalf("baseline volume list: %v", err)
	}

	portList, err := listPorts(networkClient)
	if err != nil {
		t.Fatalf("baseline port list: %v", err)
	}

	srv, vol, port = make(map[string]struct{}, len(srvList)), make(map[string]struct{}, len(volList)), make(map[string]struct{}, len(portList))

	for _, s := range srvList {
		srv[s.ID] = struct{}{}
	}

	for _, v := range volList {
		vol[v.ID] = struct{}{}
	}

	for _, p := range portList {
		port[p.ID] = struct{}{}
	}

	t.Logf("baseline: servers=%d volumes=%d ports=%d", len(srv), len(vol), len(port))

	return srv, vol, port
}

// purgeServers deletes every server whose name starts with omni-e2e — any
// e2e run's machine, identified by the request-id naming scheme
// (<machineset>-<random5>), no matter which run left it behind.
func purgeServers(t *testing.T, computeClient *gophercloud.ServiceClient) {
	t.Helper()

	srvList, err := listServers(computeClient)
	if err != nil {
		t.Logf("purge: listing servers: %v", err)

		return
	}

	for _, s := range srvList {
		if !strings.HasPrefix(s.Name, "omni-e2e") {
			continue
		}

		if err := servers.Delete(computeClient, s.ID).ExtractErr(); err != nil {
			t.Logf("purge: server %s (%s) delete failed: %v", s.ID, s.Name, err)
		} else {
			t.Logf("purge: deleted server %s (%s)", s.ID, s.Name)
		}
	}
}

// purgeVolumes first waits for the server deletion to settle (the boot
// volume has delete_on_termination=true — proven in Run 21 — and deletes
// itself), then deletes any volume created during this run, i.e. not in the
// baseline snapshot. A volume still attached to a deleting server fails with
// an error and is only logged: this is a best-effort zombie killer, the
// strict cleanup assertion lives in the main test flow.
func purgeVolumes(t *testing.T, volClient *gophercloud.ServiceClient, baseline map[string]struct{}) {
	t.Helper()

	deadline := time.Now().Add(3 * time.Minute)

	for time.Now().Before(deadline) {
		volList, err := listVolumes(volClient)
		if err != nil {
			t.Logf("purge: listing volumes: %v", err)

			break
		}

		if countNonBaseline(volList, baseline) == 0 {
			return
		}

		time.Sleep(10 * time.Second)
	}

	volList, err := listVolumes(volClient)
	if err != nil {
		t.Logf("purge: listing volumes: %v", err)

		return
	}

	for _, v := range volList {
		if _, ok := baseline[v.ID]; ok {
			continue
		}

		if err := volumes.Delete(volClient, v.ID, nil).ExtractErr(); err != nil {
			t.Logf("purge: volume %s delete failed: %v", v.ID, err)
		} else {
			t.Logf("purge: deleted volume %s", v.ID)
		}
	}
}

func countNonBaseline(volList []volumes.Volume, baseline map[string]struct{}) int {
	n := 0

	for _, v := range volList {
		if _, ok := baseline[v.ID]; !ok {
			n++
		}
	}

	return n
}

// purgePorts deletes every port whose name starts with omni-e2e (the
// provider names the port after the machine request id).
func purgePorts(t *testing.T, networkClient *gophercloud.ServiceClient) {
	t.Helper()

	portList, err := listPorts(networkClient)
	if err != nil {
		t.Logf("purge: listing ports: %v", err)

		return
	}

	for _, p := range portList {
		if !strings.HasPrefix(p.Name, "omni-e2e") {
			continue
		}

		if err := ports.Delete(networkClient, p.ID).ExtractErr(); err != nil {
			t.Logf("purge: port %s (%s) delete failed: %v", p.ID, p.Name, err)
		} else {
			t.Logf("purge: deleted port %s (%s)", p.ID, p.Name)
		}
	}
}

func containsString(haystack []string, needle string) bool {
	for _, s := range haystack {
		if s == needle {
			return true
		}
	}

	return false
}

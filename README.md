# omni-infra-provider-openstack

OpenStack infrastructure provider for [Omni](https://omni.siderolabs.com):
provisions Talos machines as Nova servers, declaratively, driven by Omni
machine classes.

Status: **v0.1.0 released** — a Talos cluster born from OpenStack via
self-hosted Omni, surviving a full host reboot (stateless provider, state
lives in Omni).

## Highlights

- **Boot from volume with AZ affinity** — all disks (boot + data) ride the
  Nova BDM train: same availability zone as the server, safe with
  `cross_az_attach=False` (the CAPO `from: Machine` lesson).
- **Automatic availability zone rotation** — `availability_zone: auto`
  reads the zones **live from Nova** and spreads machines statelessly
  (round-robin keyed by machine request id). No other Omni provider does this.
- **Server groups (anti-affinity)** — spread control planes across hosts.
- **Security groups, metadata, additional disks** — full machine shaping.
- **Per-machine join tokens** — media stays pristine
  (`WithoutConnectionParams`), every machine gets its own token via
  config drive.

## Machine Class Configuration

The `providerdata` of an auto-provision machine class follows this schema:

```yaml
config:
  flavor: SCS-2V-8              # Nova flavor name or ID
  network: my-network            # Neutron network name or ID
  security_groups:               # port security groups (egress to Omni!)
    - default
  disk_size: 20                  # boot volume size in GB
  volume_type: fast-ssd          # optional Cinder type for the boot volume
  availability_zone: auto        # zone name, "auto" (rotate live from Nova), or empty
  server_group: control-plane-spread  # optional Nova server group (spread!)
  architecture: amd64            # Talos image architecture
  metadata:                      # optional Nova server metadata
    managed-by: omni-infra-provider-openstack
  additional_disks:              # extra data volumes (e.g. Longhorn)
    - size_gb: 100
      volume_type: fast-ssd
  additional_nics:               # extra network interfaces (guild parity!)
    - network: storage-network
      security_groups: [storage]
```

## Anti-Affinity (Server Groups)

Omni does not know physical topology — spreading is infrastructure business:

    openstack server group create --policy soft-anti-affinity control-plane-spread
    openstack server group create --policy soft-anti-affinity worker-spread

## Deployment (self-hosted Omni)

Run the provider as a container next to your Omni instance (Podman/Quadlet):

1. Omni UI → Infrastructure Providers → Create (`openstack`) — copy the
   generated service account key (the birth key, not a user account!).
2. Provide `clouds.yaml` (application credential, least privilege) and the
   key via env file; point `OS_CLOUD` and `OMNI_ENDPOINT` at your realm.
3. Create a machine class (UI form comes from this provider's schema),
   then a cluster — machines are born as Nova servers.

## Commands

    make build
    ./bin/provider check-cloud --cloud <cloud>     # auth, flavors, images probe
    ./bin/provider smoke-test --cloud <cloud>      # create + fully clean up a test server
        --network S --image S [--flavor S] [--disk-size N]
        [--extra-disks N] [--server-group S]

Credentials come from `~/.config/openstack/clouds.yaml` (application
credential); the provider reads them itself — never commit them.

## Testing

Two layers, both wired into CI:

- `make test` — hermetic Tier-1 API contract tests (gophercloud fake
  transport; no OpenStack needed); runs on every PR via `ci.yml`.
- `make test-e2e` — Tier-2 end-to-end: a GitHub runner brings up DevStack
  (stable/2026.2) + self-hosted Omni (v1.13.3) + this provider, then
  `TestE2EProvisionDeprovision` provisions a machine through Omni
  (cluster → machine class → machine set), asserts the Nova server,
  boot volume, port and security group, deprovisions everything and waits
  for the baseline. Runs nightly (03:00 UTC) and on demand via
  `.github/workflows/e2e.yml`; no secrets in the repo — the runner
  generates everything it needs locally.

Local run against an existing Omni instance with the provider registered
and an OpenStack cloud (the test skips without these):

    OMNI_ENDPOINT=https://omni.example:8080 \
    OMNI_SERVICE_ACCOUNT_KEY=<key> \
    OS_CLOUD=mycloud \
    make test-e2e

Optional: `OS_CLIENT_CONFIG_FILE` (clouds.yaml path; gophercloud's
clientconfig search paths apply otherwise) and `OMNICTL_BIN` (defaults to
`omnictl` on PATH). The test sources live in `e2e/`, the pipeline in
`.github/workflows/e2e.yml`.

**Warning:** run the local e2e test against a throwaway project only.
The cleanup purges leftovers aggressively: it deletes every volume that
appeared in the project after the test started (not just this test's
own) and every server and port whose name starts with `omni-e2e` —
including resources from any parallel e2e run. On a shared project this
can destroy unrelated volumes. The CI pipeline is unaffected: it runs on
a fresh, disposable DevStack cloud.

## OCI Image

Prebuilt multi-arch image (amd64/arm64):
`ghcr.io/pjsinka/omni-infra-provider-openstack:v0.1.0`

    docker build -t omni-infra-provider-openstack:test .

Two stages: `golang:1.27-alpine` build (CGO disabled) → `alpine:3.22`
runtime with CA certificates. On the target host build natively
(`tar | podman build`) to match its architecture.

## Roadmap

- [x] Skeleton, auth, Glance streaming upload, Nova boot from volume +
      config drive join, status polling (ACTIVE/ERROR healing)
- [x] BDM data disks with AZ inheritance, server groups, metadata,
      full idempotent deprovision
- [x] Automatic AZ rotation (live from Nova)
- [x] End-to-end: self-hosted Omni + cluster born from OpenStack,
      host-reboot survival
- [x] Guild parity: additional NICs (per-port security groups);
      system extensions flow via cluster templates (Omni-native)
- [ ] Floating IPs, access-rule hardening, pb.go regeneration (protoc)
- [x] Multi-arch builds (GHCR amd64/arm64), publication (v0.1.0)
- [x] E2E suite (DevStack + Omni nightly pipeline, provision→assert→
      deprovision; two consecutive green runs)

## License

MPL-2.0

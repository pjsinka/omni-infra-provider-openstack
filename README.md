# omni-infra-provider-openstack

OpenStack infrastructure provider for [Omni](https://omni.siderolabs.com):
provisions Talos machines as Nova servers, declaratively, driven by Omni
machine classes.

Status: **proven end-to-end** — a Talos cluster born from OpenStack via
self-hosted Omni, surviving a full host reboot (stateless provider, state
lives in Omni). Brewing toward publication.

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

## OCI Image

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
- [ ] Multi-arch builds, E2E suite, publication

## License

MPL-2.0

// This Source Code Form is subject to the terms of the Mozilla Public
// License, v. 2.0. If a copy of the MPL was not distributed with this
// file, You can obtain one at http://mozilla.org/MPL/2.0/.

// Package openstack contains the OpenStack API client used by the provider.
package openstack

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"sort"
	"strings"

	"github.com/gophercloud/gophercloud"
	"github.com/gophercloud/gophercloud/openstack/compute/v2/extensions/availabilityzones"
	"github.com/gophercloud/gophercloud/openstack/compute/v2/extensions/bootfromvolume"
	"github.com/gophercloud/gophercloud/openstack/compute/v2/extensions/schedulerhints"
	"github.com/gophercloud/gophercloud/openstack/compute/v2/extensions/servergroups"
	"github.com/gophercloud/gophercloud/openstack/compute/v2/flavors"
	"github.com/gophercloud/gophercloud/openstack/compute/v2/servers"
	"github.com/gophercloud/gophercloud/openstack/imageservice/v2/imagedata"
	"github.com/gophercloud/gophercloud/openstack/imageservice/v2/images"
	"github.com/gophercloud/gophercloud/openstack/networking/v2/networks"
	"github.com/gophercloud/gophercloud/openstack/networking/v2/ports"
	"github.com/gophercloud/utils/openstack/clientconfig"
	"go.uber.org/zap"
)

// Client talks to OpenStack.
type Client struct {
	Cloud   string
	opts    *clientconfig.ClientOpts
	image   *gophercloud.ServiceClient
	compute *gophercloud.ServiceClient
	network *gophercloud.ServiceClient
	volume  *gophercloud.ServiceClient
}

// New authenticates against the named cloud from clouds.yaml and prepares
// service clients (image, compute, network, volume).
func New(cloud string) (*Client, error) {
	if cloud == "" {
		return nil, fmt.Errorf("cloud name is required (set --cloud or OS_CLOUD)")
	}

	opts := &clientconfig.ClientOpts{Cloud: cloud}

	pc, err := clientconfig.AuthenticatedClient(opts)
	if err != nil {
		return nil, fmt.Errorf("authentication with cloud %q failed: %w", cloud, err)
	}

	c := &Client{Cloud: cloud, opts: opts}

	targets := []struct {
		service string
		client  **gophercloud.ServiceClient
	}{
		{"image", &c.image},
		{"compute", &c.compute},
		{"network", &c.network},
	}

	for _, t := range targets {
		sc, svcErr := clientconfig.NewServiceClient(t.service, opts)
		if svcErr != nil {
			return nil, fmt.Errorf("creating %s client: %w", t.service, svcErr)
		}

		*t.client = sc
	}

	// Volume is resolved separately: gophercloud's
	// openstack.NewBlockStorageV3 hardcodes the legacy catalog type
	// "volumev3", but the official OpenStack service type for Cinder is
	// "block-storage" — DevStack 2026.2+ registers only the official type,
	// which makes the stock lookup fail with "no suitable endpoint". See
	// newBlockStorageClient.
	cloudCfg, cloudErr := clientconfig.GetCloudFromYAML(opts)
	if cloudErr != nil {
		return nil, fmt.Errorf("reading cloud config for %q: %w", cloud, cloudErr)
	}

	volumeOpts := gophercloud.EndpointOpts{
		Region:       cloudCfg.RegionName,
		Availability: gophercloud.Availability(cloudCfg.EndpointType),
	}

	c.volume, err = newBlockStorageClient(pc, volumeOpts)
	if err != nil {
		return nil, fmt.Errorf("creating volume client: %w", err)
	}

	// BDM tags (2.42+) and BDM volume_type (2.67+) require a newer compute
	// API microversion than gophercloud's conservative default.
	c.compute.Microversion = "2.67"

	return c, nil
}

// newBlockStorageClient resolves the Cinder (block storage) endpoint from the
// service catalog. The official OpenStack service type is "block-storage"
// (DevStack 2026.2+ registers only this type — devstack lib/cinder:
// "block-storage is the official service type"), while older clouds register
// the legacy types "volumev3"/"volume" that gophercloud's
// openstack.NewBlockStorageV3 hardcodes. Try the official type first, then
// the legacy ones, so the client works on both.
func newBlockStorageClient(pc *gophercloud.ProviderClient, eo gophercloud.EndpointOpts) (*gophercloud.ServiceClient, error) {
	var lastErr error

	for _, serviceType := range []string{"block-storage", "volumev3", "volume"} {
		sc, err := initServiceClient(pc, eo, serviceType)
		if err == nil {
			return sc, nil
		}

		lastErr = err
	}

	return nil, lastErr
}

// initServiceClient mirrors gophercloud's unexported initClientOpts: apply
// the service-type defaults, resolve the URL via the provider's endpoint
// locator and wrap it in a service client.
func initServiceClient(pc *gophercloud.ProviderClient, eo gophercloud.EndpointOpts, serviceType string) (*gophercloud.ServiceClient, error) {
	eo.ApplyDefaults(serviceType)

	url, err := pc.EndpointLocator(eo)
	if err != nil {
		return nil, err
	}

	return &gophercloud.ServiceClient{
		ProviderClient: pc,
		Endpoint:       url,
		Type:           serviceType,
	}, nil
}

// CheckCloud performs a read-only connectivity probe and returns a report.
func (c *Client) CheckCloud(_ context.Context) (string, error) {
	var sb strings.Builder

	sb.WriteString(fmt.Sprintf("auth: OK (cloud %q)\n", c.Cloud))

	flavorPager := flavors.ListDetail(c.compute, flavors.ListOpts{})

	flavorPages, err := flavorPager.AllPages()
	if err != nil {
		return "", fmt.Errorf("paging flavors: %w", err)
	}

	allFlavors, err := flavors.ExtractFlavors(flavorPages)
	if err != nil {
		return "", fmt.Errorf("extracting flavors: %w", err)
	}

	names := make([]string, 0, len(allFlavors))
	for _, f := range allFlavors {
		names = append(names, fmt.Sprintf("%s(%dvCPU/%dMB/%dGB)", f.Name, f.VCPUs, f.RAM, f.Disk))
	}

	sb.WriteString(fmt.Sprintf("flavors (%d): %s\n", len(allFlavors), strings.Join(names, ", ")))

	imagePager := images.List(c.image, images.ListOpts{Limit: 10, Sort: "name:asc"})

	imagePages, err := imagePager.AllPages()
	if err != nil {
		return "", fmt.Errorf("paging images: %w", err)
	}

	allImages, err := images.ExtractImages(imagePages)
	if err != nil {
		return "", fmt.Errorf("extracting images: %w", err)
	}

	imageNames := make([]string, 0, len(allImages))
	for _, img := range allImages {
		imageNames = append(imageNames, fmt.Sprintf("%s [%s]", img.Name, img.Status))
	}

	sb.WriteString(fmt.Sprintf("first images (%d shown): %s\n", len(allImages), strings.Join(imageNames, ", ")))

	var importInfo struct {
		ImportMethods struct {
			Value map[string][]string `json:"value"`
		} `json:"import-methods"`
	}

	_, err = c.image.Get(c.image.ServiceURL("info", "import"), nil, &gophercloud.RequestOpts{
		JSONResponse: &importInfo,
		OkCodes:      []int{200},
	})
	if err != nil {
		sb.WriteString("glance import methods: unknown (no permission to read /info/import)\n")
	} else {
		webEnabled := false
		for m := range importInfo.ImportMethods.Value {
			if m == "web-download" {
				webEnabled = true
			}
		}

		sb.WriteString(fmt.Sprintf("glance import methods: %v (web-download: %v)\n",
			keys(importInfo.ImportMethods.Value), webEnabled))
	}

	return sb.String(), nil
}

func keys(m map[string][]string) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}

	return out
}

// ImageIDByName looks up an image ID by exact name.
func (c *Client) ImageIDByName(name string) (string, error) {
	pager := images.List(c.image, images.ListOpts{Name: name, Limit: 1})

	pages, err := pager.AllPages()
	if err != nil {
		return "", fmt.Errorf("listing images: %w", err)
	}

	found, err := images.ExtractImages(pages)
	if err != nil {
		return "", fmt.Errorf("extracting images: %w", err)
	}

	if len(found) == 0 {
		return "", fmt.Errorf("image %q not found", name)
	}

	return found[0].ID, nil
}

// EnsureImage makes sure a Glance image with the given storage key exists,
// streaming the image data from the image factory URL straight into Glance
// (regular upload — works everywhere, no web-download import required).
// It returns the image ID.
func (c *Client) EnsureImage(ctx context.Context, logger *zap.Logger, storageKey, url string, headers http.Header) (string, error) {
	listPager := images.List(c.image, images.ListOpts{Name: storageKey, Limit: 1})

	listPages, err := listPager.AllPages()
	if err != nil {
		return "", fmt.Errorf("listing images: %w", err)
	}

	found, err := images.ExtractImages(listPages)
	if err != nil {
		return "", fmt.Errorf("extracting images: %w", err)
	}

	if len(found) > 0 {
		img := found[0]
		if img.Status != images.ImageStatusActive {
			// Another machine request is uploading the same image right now.
			return "", fmt.Errorf("image %q is %q, waiting for it to become active", storageKey, img.Status)
		}

		logger.Info("image already present", zap.String("image", img.ID))

		return img.ID, nil
	}

	created, err := images.Create(c.image, images.CreateOpts{
		Name:            storageKey,
		DiskFormat:      "qcow2",
		ContainerFormat: "bare",
	}).Extract()
	if err != nil {
		return "", fmt.Errorf("creating image: %w", err)
	}

	httpReq, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return "", fmt.Errorf("building image download request: %w", err)
	}

	for header, values := range headers {
		httpReq.Header[http.CanonicalHeaderKey(header)] = values
	}

	resp, err := http.DefaultClient.Do(httpReq)
	if err != nil {
		_ = images.Delete(c.image, created.ID)

		return "", fmt.Errorf("downloading image from factory: %w", err)
	}

	defer func() { _ = resp.Body.Close() }()

	if resp.StatusCode != http.StatusOK {
		_ = images.Delete(c.image, created.ID)

		return "", fmt.Errorf("unexpected status %d downloading image from factory", resp.StatusCode)
	}

	uploadRes := imagedata.Upload(c.image, created.ID, resp.Body)
	if uploadRes.Err != nil {
		_ = images.Delete(c.image, created.ID)

		return "", fmt.Errorf("uploading image data to glance: %w", uploadRes.Err)
	}

	logger.Info("uploaded image to glance",
		zap.String("image", created.ID),
		zap.String("name", storageKey),
		zap.Int64("bytes", resp.ContentLength))

	return created.ID, nil
}

// ServerRequest describes the machine to create.
type ServerRequest struct {
	Name             string
	ImageID          string
	Flavor           string
	Network          string
	SecurityGroups   []string
	DiskSizeGB       int
	VolumeType       string
	AvailabilityZone string
	ServerGroup      string
	UserData         string
	ConfigDrive      bool
	Metadata         map[string]string
	AdditionalDisks  []AdditionalDisk
	AdditionalNICs   []AdditionalNIC
}

// AdditionalNIC describes an extra Neutron port attached to the machine.
type AdditionalNIC struct {
	Network        string
	SecurityGroups []string
}

// AdditionalDisk describes an extra Cinder volume attached to the machine.
type AdditionalDisk struct {
	SizeGB     int
	VolumeType string
}

// CreateServer creates a Nova server booted from a Cinder volume created off
// the given image, attached to a freshly created Neutron port, with a config
// drive carrying the join config. It returns the server UUID.
func (c *Client) CreateServer(ctx context.Context, logger *zap.Logger, req ServerRequest) (string, error) {
	networkID, err := c.resolveNetwork(req.Network)
	if err != nil {
		return "", err
	}

	portOpts := ports.CreateOpts{
		NetworkID: networkID,
		Name:      req.Name,
	}

	if len(req.SecurityGroups) > 0 {
		portOpts.SecurityGroups = &req.SecurityGroups
	}

	port, err := ports.Create(c.network, portOpts).Extract()
	if err != nil {
		return "", fmt.Errorf("creating neutron port: %w", err)
	}

	allPorts := []string{port.ID}

	deleteAllPorts := func() {
		for _, id := range allPorts {
			_ = ports.Delete(c.network, id).ExtractErr()
		}
	}

	// Additional NICs: one extra Neutron port per entry, named
	// "<machine>-nic-<n>", cleaned up together with the machine.
	for i, nic := range req.AdditionalNICs {
		nicNetworkID, nicErr := c.resolveNetwork(nic.Network)
		if nicErr != nil {
			deleteAllPorts()

			return "", nicErr
		}

		nicPortOpts := ports.CreateOpts{
			NetworkID: nicNetworkID,
			Name:       fmt.Sprintf("%s-nic-%d", req.Name, i),
		}

		if len(nic.SecurityGroups) > 0 {
			nicPortOpts.SecurityGroups = &nic.SecurityGroups
		}

		nicPort, nicErr := ports.Create(c.network, nicPortOpts).Extract()
		if nicErr != nil {
			deleteAllPorts()

			return "", fmt.Errorf("creating additional nic port %d: %w", i, nicErr)
		}

		allPorts = append(allPorts, nicPort.ID)
	}

	flavorRef, err := c.resolveFlavor(req.Flavor)
	if err != nil {
		deleteAllPorts()

		return "", err
	}

	// Scheduler hints: optionally pin the machine to a Nova server group
	// (anti-affinity policies spread machines across compute hosts).
	var builderOpts servers.CreateOptsBuilder = baseOptsPrepared(req, flavorRef, allPorts)

	if req.ServerGroup != "" {
		groupID, grpErr := c.resolveServerGroup(req.ServerGroup)
		if grpErr != nil {
			deleteAllPorts()

			return "", grpErr
		}

		builderOpts = schedulerhints.CreateOptsExt{
			CreateOptsBuilder: builderOpts,
			SchedulerHints:    schedulerhints.SchedulerHints{Group: groupID},
		}
	}

	createOpts := bootfromvolume.CreateOptsExt{
		CreateOptsBuilder: builderOpts,
		BlockDevice:       blockDevicesPrepared(req),
	}

	server, err := servers.Create(c.compute, createOpts).Extract()
	if err != nil {
		deleteAllPorts()

		return "", fmt.Errorf("creating nova server: %w", err)
	}

	logger.Info("created server",
		zap.String("server", server.ID),
		zap.String("port", port.ID),
		zap.String("flavor", flavorRef),
		zap.Int("additional_disks", len(req.AdditionalDisks)),
		zap.Int("additional_nics", len(req.AdditionalNICs)))

	return server.ID, nil
}

// baseOptsPrepared builds the base server create options.
func baseOptsPrepared(req ServerRequest, flavorRef string, portIDs []string) servers.CreateOpts {
	configDrive := req.ConfigDrive

	networks := make([]servers.Network, 0, len(portIDs))
	for _, id := range portIDs {
		networks = append(networks, servers.Network{Port: id})
	}

	opts := servers.CreateOpts{
		Name:        req.Name,
		FlavorRef:   flavorRef,
		UserData:    []byte(req.UserData),
		Networks:    networks,
		ConfigDrive: &configDrive,
		Metadata:    req.Metadata,
	}

	if req.AvailabilityZone != "" {
		opts.AvailabilityZone = req.AvailabilityZone
	}

	return opts
}

// blockDevicesPrepared builds the block device mapping: boot volume from the
// image plus additional blank data volumes — all created by Nova in the
// server's availability zone (CAPO's "availabilityZone.from: Machine"
// lesson) and deleted with the server.
func blockDevicesPrepared(req ServerRequest) []bootfromvolume.BlockDevice {
	bootDevice := bootfromvolume.BlockDevice{
		SourceType:          bootfromvolume.SourceType("image"),
		UUID:                req.ImageID,
		BootIndex:           0,
		DeleteOnTermination: true,
		DestinationType:     bootfromvolume.DestinationType("volume"),
		VolumeSize:          req.DiskSizeGB,
	}

	if req.VolumeType != "" {
		bootDevice.VolumeType = req.VolumeType
	}

	blockDevices := []bootfromvolume.BlockDevice{bootDevice}

	for i, ad := range req.AdditionalDisks {
		device := bootfromvolume.BlockDevice{
			SourceType:          bootfromvolume.SourceType("blank"),
			BootIndex:           -1,
			DeleteOnTermination: true,
			DestinationType:     bootfromvolume.DestinationType("volume"),
			VolumeSize:          ad.SizeGB,
			Tag:                 fmt.Sprintf("data-%d", i),
		}

		if ad.VolumeType != "" {
			device.VolumeType = ad.VolumeType
		}

		blockDevices = append(blockDevices, device)
	}

	return blockDevices
}

// AvailabilityZones returns the names of all currently available Nova
// availability zones in the project's region, sorted for a deterministic
// rotation base.
func (c *Client) AvailabilityZones() ([]string, error) {
	pager := availabilityzones.List(c.compute)

	pages, err := pager.AllPages()
	if err != nil {
		return nil, fmt.Errorf("listing availability zones: %w", err)
	}

	all, err := availabilityzones.ExtractAvailabilityZones(pages)
	if err != nil {
		return nil, fmt.Errorf("extracting availability zones: %w", err)
	}

	names := make([]string, 0, len(all))
	for _, z := range all {
		if z.ZoneState.Available {
			names = append(names, z.ZoneName)
		}
	}

	sort.Strings(names)

	return names, nil
}

// resolveServerGroup resolves a server group name to its ID; a server group
// ID is passed through unchanged.
func (c *Client) resolveServerGroup(nameOrID string) (string, error) {
	pager := servergroups.List(c.compute, servergroups.ListOpts{})

	pages, err := pager.AllPages()
	if err != nil {
		return "", fmt.Errorf("listing server groups: %w", err)
	}

	allGroups, err := servergroups.ExtractServerGroups(pages)
	if err != nil {
		return "", fmt.Errorf("extracting server groups: %w", err)
	}

	for _, g := range allGroups {
		if g.Name == nameOrID {
			return g.ID, nil
		}
	}

	// No group with that name: only pass the value through if it looks like a
	// UUID — anything else is a typo (e.g. a security group name) and would
	// make Nova fail with "Group must be a UUID".
	if uuidLike(nameOrID) {
		return nameOrID, nil
	}

	return "", fmt.Errorf("server group %q not found (and not a UUID)", nameOrID)
}

// uuidLike reports whether s looks like a UUID.
func uuidLike(s string) bool {
	if len(s) != 36 {
		return false
	}

	for i, ch := range s {
		if i == 8 || i == 13 || i == 18 || i == 23 {
			if ch != '-' {
				return false
			}

			continue
		}

		isHex := (ch >= '0' && ch <= '9') || (ch >= 'a' && ch <= 'f') || (ch >= 'A' && ch <= 'F')
		if !isHex {
			return false
		}
	}

	return true
}

// ServerStatus returns the current Nova status of the server (e.g. BUILD,
// ACTIVE, ERROR). A missing server reports "DELETED".
func (c *Client) ServerStatus(_ context.Context, serverID string) (string, error) {
	server, err := servers.Get(c.compute, serverID).Extract()

	var err404 gophercloud.ErrDefault404

	if err != nil {
		if errors.As(err, &err404) {
			return "DELETED", nil
		}

		return "", fmt.Errorf("getting server: %w", err)
	}

	return string(server.Status), nil
}

// ErrCleanupPending signals that deletion is still in progress and the
// caller should retry later.
var ErrCleanupPending = errors.New("cleanup pending")

// DeleteServer deletes the Nova server and afterwards removes the port the
// provider created for it (matched by port name — Nova detaches foreign
// ports asynchronously, so device_id is not reliable). Missing resources
// are not an error; the call is idempotent.
func (c *Client) DeleteServer(ctx context.Context, logger *zap.Logger, serverID, portName string) error {
	err := servers.Delete(c.compute, serverID).ExtractErr()

	var err404 gophercloud.ErrDefault404

	if err != nil && !errors.As(err, &err404) {
		return fmt.Errorf("deleting server: %w", err)
	}

	if err == nil {
		logger.Info("requested server deletion", zap.String("server", serverID))

		return ErrCleanupPending
	}

	portPager := ports.List(c.network, ports.ListOpts{Name: portName})

	portPages, err := portPager.AllPages()
	if err != nil {
		return fmt.Errorf("listing leftover ports: %w", err)
	}

	leftPorts, err := ports.ExtractPorts(portPages)
	if err != nil {
		return fmt.Errorf("extracting leftover ports: %w", err)
	}

	for _, pt := range leftPorts {
		if delErr := ports.Delete(c.network, pt.ID).ExtractErr(); delErr != nil {
			var port404 gophercloud.ErrDefault404

			if errors.As(delErr, &port404) {
				continue
			}

			return fmt.Errorf("deleting leftover port: %w", delErr)
		}

		logger.Info("deleted leftover port", zap.String("port", pt.ID))
	}

	// Additional NIC ports ("<name>-nic-<n>"): identified by name prefix,
	// since their indices are not recorded in the state.
	nicPager := ports.List(c.network, ports.ListOpts{})

	nicPages, err := nicPager.AllPages()
	if err != nil {
		return fmt.Errorf("listing leftover nic ports: %w", err)
	}

	allNicCandidates, err := ports.ExtractPorts(nicPages)
	if err != nil {
		return fmt.Errorf("extracting leftover nic ports: %w", err)
	}

	nicPrefix := portName + "-nic-"
	leftNics := 0

	for _, pt := range allNicCandidates {
		if !strings.HasPrefix(pt.Name, nicPrefix) {
			continue
		}

		if delErr := ports.Delete(c.network, pt.ID).ExtractErr(); delErr != nil {
			var port404 gophercloud.ErrDefault404

			if errors.As(delErr, &port404) {
				continue
			}

			// still attached or transient state — retry later
			leftNics++

			continue
		}

		logger.Info("deleted leftover nic port", zap.String("port", pt.Name))
	}

	// Additional data disks were created as BDM entries with
	// delete_on_termination=true: Nova removes them together with the
	// server — no separate volume cleanup needed.

	if len(leftPorts) == 0 && leftNics == 0 {
		logger.Info("machine fully deprovisioned",
			zap.String("server", serverID),
			zap.String("port", portName))

		return nil
	}

	return ErrCleanupPending
}

// resolveNetwork resolves a network name to its ID; a network ID is passed
// through unchanged.
func (c *Client) resolveNetwork(nameOrID string) (string, error) {
	pager := networks.List(c.network, networks.ListOpts{Name: nameOrID})

	pages, err := pager.AllPages()
	if err != nil {
		return "", fmt.Errorf("listing networks: %w", err)
	}

	nets, err := networks.ExtractNetworks(pages)
	if err != nil {
		return "", fmt.Errorf("extracting networks: %w", err)
	}

	switch {
	case len(nets) == 1:
		return nets[0].ID, nil
	case len(nets) == 0:
		// Assume the value is already an ID.
		return nameOrID, nil
	default:
		return "", fmt.Errorf("network %q is ambiguous (%d matches)", nameOrID, len(nets))
	}
}

// resolveFlavor resolves a flavor name to its ID; a flavor ID is passed
// through unchanged.
func (c *Client) resolveFlavor(nameOrID string) (string, error) {
	pager := flavors.ListDetail(c.compute, flavors.ListOpts{})

	pages, err := pager.AllPages()
	if err != nil {
		return "", fmt.Errorf("listing flavors: %w", err)
	}

	allFlavors, err := flavors.ExtractFlavors(pages)
	if err != nil {
		return "", fmt.Errorf("extracting flavors: %w", err)
	}

	for _, f := range allFlavors {
		if f.Name == nameOrID {
			return f.ID, nil
		}
	}

	// No flavor with that name: assume the value is already an ID.
	return nameOrID, nil
}

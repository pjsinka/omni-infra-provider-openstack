// This Source Code Form is subject to the terms of the Mozilla Public
// License, v. 2.0. If a copy of the MPL was not distributed with this
// file, You can obtain one at http://mozilla.org/MPL/2.0/.

// Package provider implements the OpenStack infra provider core.
package provider

import (
	"context"
	"errors"
	"hash/crc32"
	"time"

	"github.com/siderolabs/omni/client/pkg/imagefactory"
	"github.com/siderolabs/omni/client/pkg/infra/provision"
	"github.com/siderolabs/omni/client/pkg/omni/resources/infra"
	"go.uber.org/zap"

	"github.com/pjsinka/omni-infra-provider-openstack/internal/pkg/provider/data"
	ospkg "github.com/pjsinka/omni-infra-provider-openstack/internal/pkg/provider/openstack"
	"github.com/pjsinka/omni-infra-provider-openstack/internal/pkg/provider/resources"
)

// errRequestNameTooLong is returned when the machine request name exceeds
// the Nova server name limit.
var errRequestNameTooLong = errors.New("the machine request name can not be longer than 63 characters")

// Provisioner implements the OpenStack infra provider.
type Provisioner struct {
	client *ospkg.Client
}

// NewProvisioner creates a new provisioner.
func NewProvisioner(client *ospkg.Client) *Provisioner {
	return &Provisioner{client: client}
}

// ProvisionSteps implements infra.Provisioner.
//
// Moon I: framework wiring; OpenStack API calls land in moon II.
func (p *Provisioner) ProvisionSteps() []provision.Step[*resources.Machine] {
	return []provision.Step[*resources.Machine]{
		provision.NewStep("validateRequest", func(_ context.Context, _ *zap.Logger, pctx provision.Context[*resources.Machine]) error {
			if len(pctx.GetRequestID()) > 62 {
				return errRequestNameTooLong
			}

			return nil
		}),
		provision.NewStep("ensureImage", func(ctx context.Context, logger *zap.Logger, pctx provision.Context[*resources.Machine]) error {
			pctx.State.TypedSpec().Value.TalosVersion = pctx.GetTalosVersion()

			var pdata data.Data

			if err := pctx.UnmarshalProviderData(&pdata); err != nil {
				return err
			}

			if err := data.ValidateFirmware(pdata.Firmware); err != nil {
				// Plain error: invalid machine class data must not trigger
				// Omni retry loops.
				return err
			}

			// Resolve the installation media through Omni (authenticated image
			// factories supported). OpenStack consumes the disk image directly via
			// Glance web-import, so no connection params travel inside the image.
			media, err := pctx.EnsureInstallationMedia(
				ctx, logger, provision.MediaSpec{
					MediaSpec: imagefactory.MediaSpec{
						Kind:         imagefactory.InstallationMediaKindDisk,
						Platform:     "openstack",
						Architecture: pdata.Architecture,
						Format:        "qcow2",
					},
					DownloadTokenTTL: time.Hour,
				},
				provision.WithoutConnectionParams(),
			)
			if err != nil {
				return provision.NewRetryErrorf(time.Second*10, "error resolving the boot asset: %w", err)
			}

			pctx.State.TypedSpec().Value.Schematic = media.SchematicID

			storageKey, imageProperties := firmwareImage(media.StorageKey, pdata.Firmware)

			imageID, err := p.client.EnsureImage(ctx, logger, storageKey, media.URL, media.Headers, imageProperties)
			if err != nil {
				return provision.NewRetryErrorf(time.Second*10, "ensuring image: %w", err)
			}

			pctx.State.TypedSpec().Value.VolumeId = imageID

			return nil
		}),
		provision.NewStep("syncMachine", func(ctx context.Context, logger *zap.Logger, pctx provision.Context[*resources.Machine]) error {
			if pctx.State.TypedSpec().Value.Uuid == "" {
				var pdata data.Data

				if err := pctx.UnmarshalProviderData(&pdata); err != nil {
					return err
				}

				additional := make([]ospkg.AdditionalDisk, 0, len(pdata.AdditionalDisks))
				for _, ad := range pdata.AdditionalDisks {
					additional = append(additional, ospkg.AdditionalDisk(ad))
				}

				nics := make([]ospkg.AdditionalNIC, 0, len(pdata.AdditionalNICs))
				for _, nic := range pdata.AdditionalNICs {
					nics = append(nics, ospkg.AdditionalNIC(nic))
				}

			availabilityZone := pdata.AvailabilityZone
			if availabilityZone == "auto" {
				zones, zoneErr := p.client.AvailabilityZones()
				if zoneErr != nil {
					return provision.NewRetryErrorf(time.Second*10, "resolving availability zones: %w", zoneErr)
				}

				if len(zones) == 0 {
					return provision.NewRetryErrorf(time.Second*10, "no availability zones available")
				}

				// Stateless round-robin: deterministic spread keyed by the
				// machine request id — different machines land in different
				// zones without any provider-side counter.
				availabilityZone = zones[int(crc32.ChecksumIEEE([]byte(pctx.GetRequestID())))%len(zones)]

				logger.Info("auto-selected availability zone",
					zap.String("zone", availabilityZone),
					zap.Strings("zones", zones))
			}

			serverID, err := p.client.CreateServer(ctx, logger, ospkg.ServerRequest{
				Name:             pctx.GetRequestID(),
				ImageID:          pctx.State.TypedSpec().Value.VolumeId,
				Flavor:           pdata.Flavor,
				Network:          pdata.Network,
				SecurityGroups:   pdata.SecurityGroups,
				DiskSizeGB:       pdata.DiskSize,
				VolumeType:       pdata.VolumeType,
				AvailabilityZone: availabilityZone,
					ServerGroup:      pdata.ServerGroup,
					UserData:         pctx.ConnectionParams.JoinConfig,
					ConfigDrive:      true,
					Metadata:         pdata.Metadata,
					AdditionalDisks:  additional,
					AdditionalNICs:   nics,
				})
				if err != nil {
					return provision.NewRetryErrorf(time.Second*10, "creating server: %w", err)
				}

				pctx.State.TypedSpec().Value.Uuid = serverID
			}

			// Nova accepts builds asynchronously: poll until the server is
			// ACTIVE, heal on ERROR, and keep waiting while it builds.
			status, err := p.client.ServerStatus(ctx, pctx.State.TypedSpec().Value.Uuid)
			if err != nil {
				return provision.NewRetryErrorf(time.Second*10, "checking server status: %w", err)
			}

			switch status {
			case "ACTIVE":
				logger.Info("machine is active", zap.String("server", pctx.State.TypedSpec().Value.Uuid))

				return nil
			case "ERROR":
				logger.Warn("server entered ERROR state, cleaning up for a fresh attempt",
					zap.String("server", pctx.State.TypedSpec().Value.Uuid))

				_ = p.client.DeleteServer(ctx, logger, pctx.State.TypedSpec().Value.Uuid, pctx.GetRequestID())
				pctx.State.TypedSpec().Value.Uuid = ""

				return provision.NewRetryErrorf(time.Second*10, "server entered ERROR state, recreating")
			case "DELETED":
				logger.Warn("server vanished unexpectedly, resetting for a fresh attempt")

				pctx.State.TypedSpec().Value.Uuid = ""

				return provision.NewRetryErrorf(time.Second*10, "server vanished, recreating")
			default:
				return provision.NewRetryInterval(time.Second * 10)
			}
		}),
	}
}

// Deprovision implements infra.Provisioner.
func (p *Provisioner) Deprovision(ctx context.Context, logger *zap.Logger, res *resources.Machine, machineRequest *infra.MachineRequest) error {
	if res.TypedSpec().Value.Uuid == "" {
		logger.Info("machine deprovisioned")

		return nil
	}

	err := p.client.DeleteServer(ctx, logger, res.TypedSpec().Value.Uuid, machineRequest.Metadata().ID())

	switch {
	case err == nil:
		return nil
	case errors.Is(err, ospkg.ErrCleanupPending):
		return provision.NewRetryInterval(time.Second * 5)
	default:
		return err
	}
}

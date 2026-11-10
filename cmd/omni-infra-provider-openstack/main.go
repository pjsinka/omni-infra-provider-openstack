// This Source Code Form is subject to the terms of the Mozilla Public
// License, v. 2.0. If a copy of the MPL was not distributed with this
// file, You can obtain one at http://mozilla.org/MPL/2.0/.

// Package main is the root cmd of the provider.
package main

import (
	"errors"
	"time"
	"context"
	_ "embed"
	"encoding/base64"
	"fmt"
	"os"
	"os/signal"
	"syscall"

	"github.com/siderolabs/omni/client/pkg/client"
	"github.com/siderolabs/omni/client/pkg/infra"
	"github.com/spf13/cobra"
	"go.uber.org/zap"
	"go.uber.org/zap/zapcore"

	"github.com/pjsinka/omni-infra-provider-openstack/internal/pkg/provider"
	"github.com/pjsinka/omni-infra-provider-openstack/internal/pkg/provider/data"
	"github.com/pjsinka/omni-infra-provider-openstack/internal/pkg/provider/meta"
	ospkg "github.com/pjsinka/omni-infra-provider-openstack/internal/pkg/provider/openstack"
	"github.com/pjsinka/omni-infra-provider-openstack/internal/version"
)

//go:embed data/icon.svg
var icon []byte

// rootCmd represents the base command when called without any subcommands.
var rootCmd = &cobra.Command{
	Use:          "provider",
	Short:        "OpenStack Omni infrastructure provider",
	Long:         `Connects to Omni as an infra provider and manages servers in OpenStack`,
	Version:      version.Tag,
	SilenceUsage: true,
	RunE: func(cmd *cobra.Command, _ []string) error {
		loggerConfig := zap.NewProductionConfig()

		logger, err := loggerConfig.Build(
			zap.AddStacktrace(zapcore.ErrorLevel),
		)
		if err != nil {
			return fmt.Errorf("failed to create logger: %w", err)
		}

		if cfg.omniAPIEndpoint == "" {
			return fmt.Errorf("omni-api-endpoint flag is not set")
		}

		openstackClient, err := ospkg.New(cfg.cloud)
		if err != nil {
			return fmt.Errorf("failed to create OpenStack client: %w", err)
		}

		provisioner := provider.NewProvisioner(openstackClient)

		ip, err := infra.NewProvider(meta.ProviderID, provisioner, infra.ProviderConfig{
			Name:        cfg.providerName,
			Description: cfg.providerDescription,
			Icon:        base64.RawStdEncoding.EncodeToString(icon),
			Schema:      string(data.Schema),
		})
		if err != nil {
			return fmt.Errorf("failed to create infra provider: %w", err)
		}

		logger.Info("starting infra provider")

		clientOptions := []client.Option{
			client.WithInsecureSkipTLSVerify(cfg.insecureSkipVerify),
		}

		if cfg.serviceAccountKey != "" {
			clientOptions = append(clientOptions, client.WithServiceAccount(cfg.serviceAccountKey))
		}

		return ip.Run(cmd.Context(), logger, infra.WithOmniEndpoint(cfg.omniAPIEndpoint), infra.WithClientOptions(
			clientOptions...,
		), infra.WithEncodeRequestIDsIntoTokens(), infra.WithVersion(version.Tag))
	},
}

var cfg struct {
	omniAPIEndpoint     string
	serviceAccountKey   string
	providerName        string
	providerDescription string
	cloud               string
	insecureSkipVerify  bool
}

// smokeTestCmd creates a server and immediately deletes it again, proving the
// whole Nova/Neutron/Cinder chain without needing Omni.
var smokeTestCmd = &cobra.Command{
	Use:   "smoke-test",
	Short: "Create and immediately delete a test server",
	RunE: func(cmd *cobra.Command, _ []string) error {
		logger := zap.NewNop()

		openstackClient, err := ospkg.New(cfg.cloud)
		if err != nil {
			return err
		}

		imageID, err := openstackClient.ImageIDByName(smokeCfg.image)
		if err != nil {
			return err
		}

		fmt.Printf("using image %q: %s\n", smokeCfg.image, imageID)

		extraDisks := make([]ospkg.AdditionalDisk, 0, smokeCfg.extraDisks)
		for i := 0; i < smokeCfg.extraDisks; i++ {
			extraDisks = append(extraDisks, ospkg.AdditionalDisk{SizeGB: 5})
		}

		serverID, err := openstackClient.CreateServer(cmd.Context(), logger, ospkg.ServerRequest{
			Name:            "omni-provider-smoke-test",
			ImageID:         imageID,
			Flavor:          smokeCfg.flavor,
			Network:         smokeCfg.network,
			DiskSizeGB:      smokeCfg.diskSize,
			ServerGroup:     smokeCfg.serverGroup,
			UserData:        "#cloud-config\n",
			ConfigDrive:     true,
			Metadata:        map[string]string{"managed-by": "omni-infra-provider-openstack"},
			AdditionalDisks: extraDisks,
		})
		if err != nil {
			return err
		}

		fmt.Printf("created server: %s\n", serverID)

		deadline := time.Now().Add(3 * time.Minute)

		for {
			delErr := openstackClient.DeleteServer(cmd.Context(), logger, serverID, "omni-provider-smoke-test")
			if delErr == nil {
				break
			}

			if !errors.Is(delErr, ospkg.ErrCleanupPending) {
				return delErr
			}

			if time.Now().After(deadline) {
				return fmt.Errorf("cleanup timed out, server %s may still exist", serverID)
			}

			time.Sleep(5 * time.Second)
		}

		fmt.Printf("deleted server (incl. volume and port): %s\n", serverID)

		return nil
	},
}

var smokeCfg struct {
	flavor      string
	network     string
	image       string
	diskSize    int
	extraDisks  int
	serverGroup string
}

// checkCloudCmd probes OpenStack connectivity without involving Omni.
var checkCloudCmd = &cobra.Command{
	Use:   "check-cloud",
	Short: "Probe OpenStack auth, endpoints and Glance import methods",
	RunE: func(cmd *cobra.Command, _ []string) error {
		openstackClient, err := ospkg.New(cfg.cloud)
		if err != nil {
			return err
		}

		report, err := openstackClient.CheckCloud(cmd.Context())
		if err != nil {
			return err
		}

		fmt.Println(report)

		return nil
	},
}

func main() {
	if err := app(); err != nil {
		os.Exit(1)
	}
}

func app() error {
	ctx, cancel := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGHUP, syscall.SIGTERM)
	defer cancel()

	return rootCmd.ExecuteContext(ctx)
}

func init() {
	rootCmd.Flags().StringVar(&cfg.omniAPIEndpoint, "omni-api-endpoint", os.Getenv("OMNI_ENDPOINT"),
		"the endpoint of the Omni API, if not set, defaults to OMNI_ENDPOINT env var.")
	rootCmd.Flags().StringVar(&meta.ProviderID, "id", meta.ProviderID, "the id of the infra provider, it is used to match the resources with the infra provider label.")
	rootCmd.Flags().StringVar(&cfg.serviceAccountKey, "omni-service-account-key", os.Getenv("OMNI_SERVICE_ACCOUNT_KEY"),
		"Omni service account key, if not set, defaults to OMNI_SERVICE_ACCOUNT_KEY.")
	rootCmd.Flags().StringVar(&cfg.providerName, "provider-name", "OpenStack", "provider name as it appears in Omni")
	rootCmd.Flags().StringVar(&cfg.providerDescription, "provider-description", "OpenStack infrastructure provider", "Provider description as it appears in Omni")
	rootCmd.PersistentFlags().StringVar(&cfg.cloud, "cloud", os.Getenv("OS_CLOUD"),
		"name of the cloud in clouds.yaml (application credential), defaults to OS_CLOUD env var.")
	rootCmd.Flags().BoolVar(&cfg.insecureSkipVerify, "insecure-skip-verify", false, "ignores untrusted certs on Omni side")

	smokeTestCmd.Flags().StringVar(&smokeCfg.flavor, "flavor", "SCS-2V-8", "flavor name or ID for the smoke test server")
	smokeTestCmd.Flags().StringVar(&smokeCfg.network, "network", "", "network name or ID for the smoke test server (required)")
	smokeTestCmd.Flags().StringVar(&smokeCfg.image, "image", "", "existing glance image name for the smoke test server (required)")
	_ = smokeTestCmd.MarkFlagRequired("network")
	_ = smokeTestCmd.MarkFlagRequired("image")
	smokeTestCmd.Flags().IntVar(&smokeCfg.diskSize, "disk-size", 10, "boot volume size in GB for the smoke test server")
	smokeTestCmd.Flags().IntVar(&smokeCfg.extraDisks, "extra-disks", 1, "number of additional 5GB data volumes to attach and clean up")
	smokeTestCmd.Flags().StringVar(&smokeCfg.serverGroup, "server-group", "", "optional Nova server group (name or ID) to pin the smoke test server to")

	rootCmd.AddCommand(checkCloudCmd)
	rootCmd.AddCommand(smokeTestCmd)
}

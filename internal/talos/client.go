package talos

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"encoding/base64"
	"fmt"
	"strings"
	"time"

	"github.com/cosi-project/runtime/pkg/resource"
	machineapi "github.com/siderolabs/talos/pkg/machinery/api/machine"
	"github.com/siderolabs/talos/pkg/machinery/client"
	"github.com/siderolabs/talos/pkg/machinery/config/configpatcher"
	configres "github.com/siderolabs/talos/pkg/machinery/resources/config"
	runtimeres "github.com/siderolabs/talos/pkg/machinery/resources/runtime"
)

// callTimeout bounds every Talos API call so a hung node cannot block a reconcile worker.
const callTimeout = 30 * time.Second

// Client wraps Talos API client operations
type Client struct {
	client *client.Client
}

// nodeCtx targets a single node directly (unlike deprecated WithNodes, which proxies via apid)
// and applies callTimeout.
func nodeCtx(ctx context.Context, nodeName string) (context.Context, context.CancelFunc) {
	ctx, cancel := context.WithTimeout(ctx, callTimeout)
	return client.WithNode(ctx, nodeName), cancel
}

// Config holds Talos API connection details; the certificates are PEM, raw or base64-encoded.
type Config struct {
	Endpoints  []string
	CACert     string
	ClientCert string
	ClientKey  string
}

// NewClient creates a new Talos client
func NewClient(config *Config) (*Client, error) {
	if config == nil {
		return nil, fmt.Errorf("config is nil")
	}

	if len(config.Endpoints) == 0 {
		return nil, fmt.Errorf("no Talos endpoints provided")
	}

	tlsConfig, err := createTLSConfig(config)
	if err != nil {
		return nil, fmt.Errorf("failed to create TLS config: %w", err)
	}

	opts := []client.OptionFunc{
		client.WithEndpoints(config.Endpoints...),
		client.WithTLSConfig(tlsConfig),
	}

	talosClient, err := client.New(context.Background(), opts...)
	if err != nil {
		return nil, fmt.Errorf("failed to create Talos client: %w", err)
	}

	return &Client{client: talosClient}, nil
}

// GetVersion retrieves the Talos version from a node
func (c *Client) GetVersion(ctx context.Context, nodeName string) (string, error) {
	if c.client == nil {
		return "", fmt.Errorf("client not initialized")
	}

	ctx, cancel := nodeCtx(ctx, nodeName)
	defer cancel()

	resp, err := c.client.Version(ctx)
	if err != nil {
		return "", fmt.Errorf("failed to get version from node %s: %w", nodeName, err)
	}

	for _, msg := range resp.Messages {
		if msg.Version != nil && msg.Version.Tag != "" {
			return msg.Version.Tag, nil
		}
	}

	return "", fmt.Errorf("no version response received from node %s", nodeName)
}

// CheckConnection verifies the configured endpoints are reachable and accept the client
// credentials, without targeting a specific node.
func (c *Client) CheckConnection(ctx context.Context) error {
	if c.client == nil {
		return fmt.Errorf("client not initialized")
	}

	ctx, cancel := context.WithTimeout(ctx, callTimeout)
	defer cancel()

	if _, err := c.client.Version(ctx); err != nil {
		return fmt.Errorf("failed to query Talos endpoint: %w", err)
	}

	return nil
}

// GetSchematicID returns the factory schematic ID the node is currently running, or an empty
// string if the node was not installed from a factory image.
func (c *Client) GetSchematicID(ctx context.Context, nodeName string) (string, error) {
	if c.client == nil {
		return "", fmt.Errorf("client not initialized")
	}

	ctx, cancel := nodeCtx(ctx, nodeName)
	defer cancel()

	items, err := c.client.COSI.List(ctx, resource.NewMetadata(runtimeres.NamespaceName, runtimeres.ExtensionStatusType, "", resource.VersionUndefined))
	if err != nil {
		return "", fmt.Errorf("failed to list extensions on node %s: %w", nodeName, err)
	}

	// Talos reports the schematic as a pseudo-extension named "schematic" whose version is the ID.
	for _, item := range items.Items {
		ext, ok := item.(*runtimeres.ExtensionStatus)
		if !ok {
			continue
		}
		if spec := ext.TypedSpec(); spec.Metadata.Name == "schematic" {
			return spec.Metadata.Version, nil
		}
	}

	return "", nil
}

// Upgrade initiates an OS upgrade on a node
func (c *Client) Upgrade(ctx context.Context, nodeName, image string) error {
	if c.client == nil {
		return fmt.Errorf("client not initialized")
	}

	ctx, cancel := nodeCtx(ctx, nodeName)
	defer cancel()

	// TODO: migrate to LifecycleClient's streaming Upgrade RPC once adopted across the codebase
	//nolint:staticcheck // SA1019: UpgradeWithOptions deprecated in favor of LifecycleClient
	resp, err := c.client.UpgradeWithOptions(
		ctx,
		client.WithUpgradeImage(image),
		client.WithUpgradePreserve(true),
		client.WithUpgradeStage(false),
		client.WithUpgradeForce(false),
	)
	if err != nil {
		return fmt.Errorf("upgrade failed for node %s: %w", nodeName, err)
	}

	if len(resp.Messages) == 0 {
		return fmt.Errorf("no response received from node %s", nodeName)
	}

	return nil
}

// PatchKubeletVersion patches the kubelet image on a node via a machine config patch. Unlike
// Upgrade, this does not reboot the node - Talos restarts the kubelet service in place.
func (c *Client) PatchKubeletVersion(ctx context.Context, nodeName, kubeletImage string) error {
	patch := fmt.Sprintf(`[{"op": "add", "path": "/machine/kubelet/image", "value": %q}]`, kubeletImage)
	return c.patchMachineConfig(ctx, nodeName, patch)
}

// PatchControlPlaneVersion patches the kube-apiserver, kube-controller-manager and kube-scheduler
// static pod images on a control plane node via a machine config patch. Does not reboot the node -
// Talos restarts the static pods in place.
func (c *Client) PatchControlPlaneVersion(ctx context.Context, nodeName, apiServerImage, controllerManagerImage, schedulerImage string) error {
	patch := fmt.Sprintf(`[
		{"op": "add", "path": "/cluster/apiServer/image", "value": %q},
		{"op": "add", "path": "/cluster/controllerManager/image", "value": %q},
		{"op": "add", "path": "/cluster/scheduler/image", "value": %q}
	]`, apiServerImage, controllerManagerImage, schedulerImage)
	return c.patchMachineConfig(ctx, nodeName, patch)
}

// patchMachineConfig applies a JSON6902 patch to a node's machine config without a reboot. It
// assumes the patched fields' parent objects (e.g. machine.kubelet, cluster.apiServer) already
// exist in the node's config, which is the case for any config generated with `talosctl gen config`.
func (c *Client) patchMachineConfig(ctx context.Context, nodeName, jsonPatch string) error {
	if c.client == nil {
		return fmt.Errorf("client not initialized")
	}

	ctx, cancel := nodeCtx(ctx, nodeName)
	defer cancel()

	res, err := c.client.COSI.Get(ctx, resource.NewMetadata(configres.NamespaceName, configres.MachineConfigType, configres.ActiveID, resource.VersionUndefined))
	if err != nil {
		return fmt.Errorf("failed to get machine config from node %s: %w", nodeName, err)
	}

	mc, ok := res.(*configres.MachineConfig)
	if !ok {
		return fmt.Errorf("unexpected resource type for machine config on node %s", nodeName)
	}

	currentBytes, err := mc.Provider().Bytes()
	if err != nil {
		return fmt.Errorf("failed to read machine config from node %s: %w", nodeName, err)
	}

	patches, err := configpatcher.LoadPatches([]string{jsonPatch})
	if err != nil {
		return fmt.Errorf("failed to load config patch: %w", err)
	}

	out, err := configpatcher.Apply(configpatcher.WithBytes(currentBytes), patches)
	if err != nil {
		return fmt.Errorf("failed to apply config patch: %w", err)
	}

	patchedBytes, err := out.Bytes()
	if err != nil {
		return fmt.Errorf("failed to encode patched machine config: %w", err)
	}

	if _, err := c.client.ApplyConfiguration(ctx, &machineapi.ApplyConfigurationRequest{
		Data: patchedBytes,
		Mode: machineapi.ApplyConfigurationRequest_NO_REBOOT,
	}); err != nil {
		return fmt.Errorf("failed to apply machine config to node %s: %w", nodeName, err)
	}

	return nil
}

// Close closes the Talos client connection
func (c *Client) Close() error {
	if c.client != nil {
		return c.client.Close()
	}
	return nil
}

// decodePEM accepts raw PEM (kubectl --from-file) or base64-encoded PEM.
func decodePEM(s string) ([]byte, error) {
	if strings.HasPrefix(strings.TrimSpace(s), "-----BEGIN") {
		return []byte(s), nil
	}
	return base64.StdEncoding.DecodeString(s)
}

// createTLSConfig creates a TLS configuration from the Talos config
func createTLSConfig(config *Config) (*tls.Config, error) {
	caCertData, err := decodePEM(config.CACert)
	if err != nil {
		return nil, fmt.Errorf("failed to decode CA cert from base64: %w", err)
	}

	caCertPool := x509.NewCertPool()
	if !caCertPool.AppendCertsFromPEM(caCertData) {
		return nil, fmt.Errorf("failed to append CA cert to pool")
	}

	clientCertData, err := decodePEM(config.ClientCert)
	if err != nil {
		return nil, fmt.Errorf("failed to decode client cert from base64: %w", err)
	}

	clientKeyData, err := decodePEM(config.ClientKey)
	if err != nil {
		return nil, fmt.Errorf("failed to decode client key from base64: %w", err)
	}

	clientCert, err := tls.X509KeyPair(clientCertData, clientKeyData)
	if err != nil {
		return nil, fmt.Errorf("failed to load client cert/key: %w", err)
	}

	return &tls.Config{
		RootCAs:      caCertPool,
		Certificates: []tls.Certificate{clientCert},
		MinVersion:   tls.VersionTLS12,
	}, nil
}

package talos

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"encoding/base64"
	"fmt"

	"github.com/siderolabs/talos/pkg/machinery/client"
	kangalpatchv1alpha1 "github.com/uozalp/kangal-patch/api/v1alpha1"
)

// Client wraps Talos API client operations
type Client struct {
	client *client.Client
}

// NewClient creates a new Talos client
func NewClient(config *kangalpatchv1alpha1.TalosConfig) (*Client, error) {
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

	ctx = client.WithNode(ctx, nodeName)

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

// Upgrade initiates an OS upgrade on a node
func (c *Client) Upgrade(ctx context.Context, nodeName, image string) error {
	if c.client == nil {
		return fmt.Errorf("client not initialized")
	}

	// WithNode targets a single node directly, unlike deprecated WithNodes which proxies via apid
	ctx = client.WithNode(ctx, nodeName)

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

// IsResponsive checks if a node is responsive via Talos API
func (c *Client) IsResponsive(ctx context.Context, nodeName string) (bool, error) {
	if c.client == nil {
		return false, fmt.Errorf("client not initialized")
	}

	// WithNode targets a single node directly, unlike deprecated WithNodes which proxies via apid
	ctx = client.WithNode(ctx, nodeName)

	_, err := c.client.Version(ctx)
	if err != nil {
		return false, nil // Node not responsive, not an error condition
	}

	return true, nil
}

// Close closes the Talos client connection
func (c *Client) Close() error {
	if c.client != nil {
		return c.client.Close()
	}
	return nil
}

// createTLSConfig creates a TLS configuration from the Talos config
func createTLSConfig(config *kangalpatchv1alpha1.TalosConfig) (*tls.Config, error) {
	caCertData, err := base64.StdEncoding.DecodeString(config.CACert)
	if err != nil {
		return nil, fmt.Errorf("failed to decode CA cert from base64: %w", err)
	}

	caCertPool := x509.NewCertPool()
	if !caCertPool.AppendCertsFromPEM(caCertData) {
		return nil, fmt.Errorf("failed to append CA cert to pool")
	}

	clientCertData, err := base64.StdEncoding.DecodeString(config.ClientCert)
	if err != nil {
		return nil, fmt.Errorf("failed to decode client cert from base64: %w", err)
	}

	clientKeyData, err := base64.StdEncoding.DecodeString(config.ClientKey)
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

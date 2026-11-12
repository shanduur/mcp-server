// This Source Code Form is subject to the terms of the Mozilla Public
// License, v. 2.0. If a copy of the MPL was not distributed with this
// file, You can obtain one at http://mozilla.org/MPL/2.0/.

// Package omni implements Omni tools.
package omni

import (
	"context"
	"fmt"
	"log"
	"os"
	"strings"
	"time"

	"github.com/cosi-project/runtime/pkg/state"
	"github.com/modelcontextprotocol/go-sdk/mcp"
	"github.com/siderolabs/omni/client/pkg/client"
	"github.com/siderolabs/omni/client/pkg/client/management"
	"github.com/siderolabs/omni/client/pkg/template/operations"

	"github.com/siderolabs/mcp-server/pkg/tools"
)

// NewClient creates an Omni client from OMNI_ENDPOINT and OMNI_SERVICE_ACCOUNT_KEY.
//
// It returns a nil client when they are not set.
func NewClient() (*client.Client, error) {
	endpoint := os.Getenv("OMNI_ENDPOINT")
	key := os.Getenv("OMNI_SERVICE_ACCOUNT_KEY")

	if endpoint == "" || key == "" {
		log.Print("OMNI_ENDPOINT/OMNI_SERVICE_ACCOUNT_KEY not set, Omni access disabled")

		return nil, nil //nolint:nilnil
	}

	c, err := client.New(endpoint, client.WithServiceAccount(key))
	if err != nil {
		return nil, fmt.Errorf("failed to create omni client: %w", err)
	}

	return c, nil
}

type clusterInput struct {
	Cluster string `json:"cluster" jsonschema:"cluster name"`
}

type kubeconfigInput struct {
	Cluster string `json:"cluster" jsonschema:"cluster name"`
	TTL     string `json:"ttl,omitempty" jsonschema:"service account token TTL as Go duration, default 1h"`
}

type templateInput struct {
	Template string `json:"template" jsonschema:"cluster template YAML (multi-document)"`
	DryRun   bool   `json:"dry_run,omitempty" jsonschema:"only report the changes, do not apply them"`
}

// Register adds the Omni tools to the server.
func Register(server *mcp.Server, c *client.Client) {
	st := c.Omni().State()

	mcp.AddTool(server, &mcp.Tool{
		Name:        "omni_get",
		Description: "Get or list Omni COSI resources as YAML, equivalent to `omnictl get`. Use type 'resourcedefinitions' to discover available types.",
		Annotations: &mcp.ToolAnnotations{ReadOnlyHint: true},
	}, func(ctx context.Context, _ *mcp.CallToolRequest, in tools.ResourceQuery) (*mcp.CallToolResult, any, error) {
		out, err := tools.GetResources(ctx, st, in)
		if err != nil {
			return nil, nil, err
		}

		return tools.Text(out), nil, nil
	})

	mcp.AddTool(server, &mcp.Tool{
		Name:        "omni_talosconfig",
		Description: "Get talosconfig for a cluster managed by Omni.",
		Annotations: &mcp.ToolAnnotations{ReadOnlyHint: true},
	}, func(ctx context.Context, _ *mcp.CallToolRequest, in clusterInput) (*mcp.CallToolResult, any, error) {
		cfg, err := c.Management().WithCluster(in.Cluster).Talosconfig(ctx)
		if err != nil {
			return nil, nil, err
		}

		return tools.Text(string(cfg)), nil, nil
	})

	mcp.AddTool(server, &mcp.Tool{
		Name:        "omni_kubeconfig",
		Description: "Get a service-account kubeconfig for a cluster managed by Omni.",
		Annotations: &mcp.ToolAnnotations{ReadOnlyHint: true},
	}, func(ctx context.Context, _ *mcp.CallToolRequest, in kubeconfigInput) (*mcp.CallToolResult, any, error) {
		ttl := time.Hour

		if in.TTL != "" {
			var err error

			if ttl, err = time.ParseDuration(in.TTL); err != nil {
				return nil, nil, fmt.Errorf("invalid ttl: %w", err)
			}
		}

		cfg, err := c.Management().WithCluster(in.Cluster).Kubeconfig(ctx,
			management.WithServiceAccount(ttl, "siderolabs-mcp-server", "system:masters"),
		)
		if err != nil {
			return nil, nil, err
		}

		return tools.Text(string(cfg)), nil, nil
	})

	mcp.AddTool(server, &mcp.Tool{
		Name:        "omni_template_sync",
		Description: "Create or update a cluster from an Omni cluster template, equivalent to `omnictl cluster template sync`.",
		Annotations: &mcp.ToolAnnotations{DestructiveHint: new(true)},
	}, func(ctx context.Context, _ *mcp.CallToolRequest, in templateInput) (*mcp.CallToolResult, any, error) {
		out, err := syncTemplate(ctx, st, in)
		if err != nil {
			return nil, nil, err
		}

		return tools.Text(out), nil, nil
	})
}

func syncTemplate(ctx context.Context, st state.State, in templateInput) (string, error) {
	// confine template file references (patches etc.) to the working directory
	root, err := os.OpenRoot(".")
	if err != nil {
		return "", err
	}

	defer root.Close() //nolint:errcheck

	var sb strings.Builder

	err = operations.SyncTemplate(ctx, strings.NewReader(in.Template), &sb, st, operations.SyncOptions{DryRun: in.DryRun, Verbose: true}, root)

	return sb.String(), err
}

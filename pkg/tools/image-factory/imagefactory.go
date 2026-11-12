// This Source Code Form is subject to the terms of the Mozilla Public
// License, v. 2.0. If a copy of the MPL was not distributed with this
// file, You can obtain one at http://mozilla.org/MPL/2.0/.

// Package imagefactory implements Image Factory tools.
package imagefactory

import (
	"context"
	"encoding/json"
	"fmt"
	"net/url"

	"github.com/modelcontextprotocol/go-sdk/mcp"
	"github.com/siderolabs/image-factory/pkg/client"
	"github.com/siderolabs/image-factory/pkg/schematic"

	"github.com/siderolabs/mcp-server/pkg/tools"
)

type talosVersionInput struct {
	TalosVersion string `json:"talos_version" jsonschema:"Talos version, e.g. v1.11.0"`
}

type schematicCreateInput struct {
	Schematic string `json:"schematic" jsonschema:"schematic YAML (customization.systemExtensions, extraKernelArgs, overlay, ...)"`
}

type schematicGetInput struct {
	ID string `json:"id" jsonschema:"schematic ID"`
}

func jsonText(v any) (*mcp.CallToolResult, any, error) {
	out, err := json.MarshalIndent(v, "", "  ")
	if err != nil {
		return nil, nil, err
	}

	return tools.Text(string(out)), nil, nil
}

// Register adds the Image Factory tools for the factory at baseURL to the server.
func Register(server *mcp.Server, baseURL string) error {
	c, err := client.New(baseURL)
	if err != nil {
		return fmt.Errorf("failed to create image factory client: %w", err)
	}

	readOnly := &mcp.ToolAnnotations{ReadOnlyHint: true}

	mcp.AddTool(server, &mcp.Tool{
		Name:        "factory_versions",
		Description: "List Talos versions available in Image Factory.",
		Annotations: readOnly,
	}, func(ctx context.Context, _ *mcp.CallToolRequest, _ struct{}) (*mcp.CallToolResult, any, error) {
		v, err := c.Versions(ctx)
		if err != nil {
			return nil, nil, err
		}

		return jsonText(v)
	})

	mcp.AddTool(server, &mcp.Tool{
		Name:        "factory_extensions",
		Description: "List system extensions and overlays available in Image Factory for a Talos version.",
		Annotations: readOnly,
	}, func(ctx context.Context, _ *mcp.CallToolRequest, in talosVersionInput) (*mcp.CallToolResult, any, error) {
		ext, err := c.ExtensionsVersions(ctx, in.TalosVersion)
		if err != nil {
			return nil, nil, err
		}

		overlays, err := c.OverlaysVersions(ctx, in.TalosVersion)
		if err != nil {
			return nil, nil, err
		}

		return jsonText(map[string]any{"extensions": ext, "overlays": overlays})
	})

	mcp.AddTool(server, &mcp.Tool{
		Name:        "factory_schematic_get",
		Description: "Get a schematic from Image Factory by ID.",
		Annotations: readOnly,
	}, func(ctx context.Context, _ *mcp.CallToolRequest, in schematicGetInput) (*mcp.CallToolResult, any, error) {
		s, err := c.SchematicGet(ctx, in.ID)
		if err != nil {
			return nil, nil, err
		}

		out, err := s.Marshal()
		if err != nil {
			return nil, nil, err
		}

		return tools.Text(string(out)), nil, nil
	})

	// Schematics are content-addressed, so creating one is idempotent.
	mcp.AddTool(server, &mcp.Tool{
		Name:        "factory_schematic_create",
		Description: "Upload a schematic to Image Factory and return its ID along with the installer image and download base URL.",
		Annotations: &mcp.ToolAnnotations{IdempotentHint: true, DestructiveHint: new(false)},
	}, func(ctx context.Context, _ *mcp.CallToolRequest, in schematicCreateInput) (*mcp.CallToolResult, any, error) {
		s, err := schematic.Unmarshal([]byte(in.Schematic))
		if err != nil {
			return nil, nil, err
		}

		id, _, err := c.SchematicCreate(ctx, *s)
		if err != nil {
			return nil, nil, err
		}

		u, err := url.Parse(c.BaseURL())
		if err != nil {
			return nil, nil, err
		}

		return tools.Text(fmt.Sprintf("id: %s\ninstaller: %s/installer/%s:<talos_version>\ndownloads: %s/image/%s/<talos_version>/\n",
			id, u.Host, id, c.BaseURL(), id)), nil, nil
	})

	return nil
}

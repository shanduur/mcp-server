// This Source Code Form is subject to the terms of the Mozilla Public
// License, v. 2.0. If a copy of the MPL was not distributed with this
// file, You can obtain one at http://mozilla.org/MPL/2.0/.

// Package cmd implements the siderolabs-mcp-server command line.
package cmd

import (
	"context"
	"os"
	"os/signal"
	"syscall"

	"github.com/modelcontextprotocol/go-sdk/mcp"
	"github.com/urfave/cli/v3"

	"github.com/siderolabs/mcp-server/internal/version"
	"github.com/siderolabs/mcp-server/pkg/policy"
	imagefactory "github.com/siderolabs/mcp-server/pkg/tools/image-factory"
	"github.com/siderolabs/mcp-server/pkg/tools/omni"
	"github.com/siderolabs/mcp-server/pkg/tools/talos"
)

const name = "siderolabs-mcp-server"

// Execute runs the command line until it finishes or the process is interrupted.
func Execute() error {
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	root := &cli.Command{
		Name:    name,
		Usage:   "MCP server for Omni, Talos and Image Factory; serves over stdio unless a subcommand is given",
		Version: version.Tag,
		Flags: []cli.Flag{
			&cli.StringFlag{Name: "policy", Usage: "policy file restricting tool calls (default: built-in read-only policy)"},
			&cli.StringFlag{Name: "talos-context", Usage: "talosconfig context for direct Talos access (default: current context)"},
			&cli.StringFlag{Name: "image-factory-url", Value: "https://factory.talos.dev", Usage: "Image Factory base URL"},
		},
		Action: func(ctx context.Context, cmd *cli.Command) error {
			return run(ctx, cmd, func(server *mcp.Server) error {
				return server.Run(ctx, &mcp.StdioTransport{})
			})
		},
		Commands: []*cli.Command{serveCommand()},
	}

	return root.Run(ctx, os.Args)
}

// run builds the MCP server from the configured backends and policy, and passes it to serve.
func run(ctx context.Context, cmd *cli.Command, serve func(*mcp.Server) error) error {
	pol, err := policy.Load(cmd.String("policy"))
	if err != nil {
		return err
	}

	server := mcp.NewServer(&mcp.Implementation{Name: name, Version: version.Tag}, nil)
	server.AddReceivingMiddleware(pol.Middleware)

	var routes talos.Backends

	if routes.Omni, err = omni.NewClient(); err != nil {
		return err
	}

	if routes.Omni != nil {
		defer routes.Omni.Close() //nolint:errcheck

		omni.Register(server, routes.Omni)
	}

	if routes.Direct = talos.NewClient(ctx, cmd.String("talos-context")); routes.Direct != nil {
		defer routes.Direct.Close() //nolint:errcheck
	}

	if routes.Omni != nil || routes.Direct != nil {
		talos.Register(server, routes)
	}

	if err = imagefactory.Register(server, cmd.String("image-factory-url")); err != nil {
		return err
	}

	return serve(server)
}

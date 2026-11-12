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

	"github.com/siderolabs/mcp-server/pkg/policy"
	"github.com/siderolabs/mcp-server/pkg/tools/omni"
)

const name = "siderolabs-mcp-server"

// Execute runs the command line until it finishes or the process is interrupted.
func Execute(version string) error {
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	root := &cli.Command{
		Name:    name,
		Usage:   "MCP server for Omni; serves over stdio unless a subcommand is given",
		Version: version,
		Flags: []cli.Flag{
			&cli.StringFlag{Name: "policy", Usage: "policy file restricting tool calls (default: built-in read-only policy)"},
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
func run(_ context.Context, cmd *cli.Command, serve func(*mcp.Server) error) error {
	pol, err := policy.Load(cmd.String("policy"))
	if err != nil {
		return err
	}

	server := mcp.NewServer(&mcp.Implementation{Name: name, Version: cmd.Root().Version}, nil)
	server.AddReceivingMiddleware(pol.Middleware)

	c, err := omni.NewClient()
	if err != nil {
		return err
	}

	if c != nil {
		defer c.Close() //nolint:errcheck

		omni.Register(server, c)
	}

	return serve(server)
}

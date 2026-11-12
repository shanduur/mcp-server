// This Source Code Form is subject to the terms of the Mozilla Public
// License, v. 2.0. If a copy of the MPL was not distributed with this
// file, You can obtain one at http://mozilla.org/MPL/2.0/.

package cmd

import (
	"context"
	"crypto/subtle"
	"errors"
	"fmt"
	"log"
	"net"
	"net/http"
	"os"
	"time"

	"github.com/modelcontextprotocol/go-sdk/auth"
	"github.com/modelcontextprotocol/go-sdk/mcp"
	"github.com/urfave/cli/v3"
)

func serveCommand() *cli.Command {
	return &cli.Command{
		Name:  "serve",
		Usage: "serve MCP over streamable HTTP at /mcp; clients authenticate with the bearer token from MCP_AUTH_TOKEN",
		Flags: []cli.Flag{
			&cli.StringFlag{Name: "listen", Value: "127.0.0.1:8080", Usage: "listen address; non-loopback requires MCP_AUTH_TOKEN"},
		},
		Action: func(ctx context.Context, cmd *cli.Command) error {
			return run(ctx, cmd, func(server *mcp.Server) error {
				return serveHTTP(ctx, server, cmd.String("listen"), os.Getenv("MCP_AUTH_TOKEN"))
			})
		},
	}
}

// checkListen refuses to expose the server beyond loopback without authentication:
// it acts with the Omni service account and talosconfig credentials.
func checkListen(addr, token string) error {
	host, _, err := net.SplitHostPort(addr)
	if err != nil {
		return fmt.Errorf("invalid listen address: %w", err)
	}

	if ip := net.ParseIP(host); token == "" && host != "localhost" && (ip == nil || !ip.IsLoopback()) {
		return fmt.Errorf("refusing to listen on %q without MCP_AUTH_TOKEN", addr)
	}

	return nil
}

// serveHTTP serves the MCP streamable HTTP transport at /mcp until ctx is done.
func serveHTTP(ctx context.Context, server *mcp.Server, addr, token string) error {
	if err := checkListen(addr, token); err != nil {
		return err
	}

	var handler http.Handler = mcp.NewStreamableHTTPHandler(func(*http.Request) *mcp.Server { return server }, nil)

	if token != "" {
		handler = auth.RequireBearerToken(func(_ context.Context, got string, _ *http.Request) (*auth.TokenInfo, error) {
			if subtle.ConstantTimeCompare([]byte(got), []byte(token)) != 1 {
				return nil, auth.ErrInvalidToken
			}

			return &auth.TokenInfo{}, nil
		}, &auth.RequireBearerTokenOptions{AllowMissingExpiration: true})(handler)
	} else {
		log.Printf("MCP_AUTH_TOKEN not set, serving %s without authentication", addr)
	}

	mux := http.NewServeMux()
	mux.Handle("/mcp", handler)

	srv := &http.Server{Addr: addr, Handler: mux, ReadHeaderTimeout: 10 * time.Second}

	go func() {
		<-ctx.Done()

		shutdownCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()

		srv.Shutdown(shutdownCtx) //nolint:errcheck,contextcheck
	}()

	log.Printf("serving MCP on http://%s/mcp", addr)

	if err := srv.ListenAndServe(); !errors.Is(err, http.ErrServerClosed) {
		return err
	}

	return nil
}

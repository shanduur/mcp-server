// This Source Code Form is subject to the terms of the Mozilla Public
// License, v. 2.0. If a copy of the MPL was not distributed with this
// file, You can obtain one at http://mozilla.org/MPL/2.0/.

// Package talos implements Talos tools, reaching nodes either directly or through Omni.
package talos

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"log"
	"os"
	"strings"
	"sync"

	"github.com/modelcontextprotocol/go-sdk/mcp"
	"github.com/siderolabs/omni/client/pkg/client"
	"github.com/siderolabs/talos/pkg/machinery/api/common"
	talosclient "github.com/siderolabs/talos/pkg/machinery/client"
	clientconfig "github.com/siderolabs/talos/pkg/machinery/client/config"
	"github.com/siderolabs/talos/pkg/machinery/constants"
	"google.golang.org/protobuf/encoding/protojson"

	"github.com/siderolabs/mcp-server/pkg/tools"
)

// NewClient creates a Talos client from the default talosconfig, optionally for a named context.
//
// It returns a nil client when no usable talosconfig is found.
func NewClient(ctx context.Context, contextName string) *talosclient.Client {
	opts := []talosclient.OptionFunc{talosclient.WithDefaultConfig()}

	if contextName != "" {
		opts = append(opts, talosclient.WithContextName(contextName))
	}

	c, err := talosclient.New(ctx, opts...)
	if err != nil {
		log.Printf("no usable talosconfig (%v), direct Talos access disabled", err)

		return nil
	}

	return c
}

// Backends holds the available routes to Talos API; nil means disabled.
type Backends struct {
	// Omni routes requests through the Omni Talos API proxy.
	Omni *client.Client
	// Direct uses the local talosconfig.
	Direct *talosclient.Client
}

type backends struct {
	omni   *omniTalos
	direct *talosclient.Client
}

// omniTalos reaches Talos nodes through the Omni Talos API proxy.
type omniTalos struct {
	c   *client.Client
	cfg *clientconfig.Config
	key string
	mu  sync.Mutex
}

// config fetches the instance talosconfig from Omni once; failures are retried on the next call.
func (o *omniTalos) config(ctx context.Context) (*clientconfig.Config, error) {
	o.mu.Lock()
	defer o.mu.Unlock()

	if o.cfg != nil {
		return o.cfg, nil
	}

	raw, err := o.c.Management().Talosconfig(ctx)
	if err != nil {
		return nil, fmt.Errorf("failed to get talosconfig from Omni: %w", err)
	}

	if o.cfg, err = clientconfig.FromBytes(raw); err != nil {
		return nil, err
	}

	return o.cfg, nil
}

// client returns a Talos client for the target and a func to release it.
func (b backends) client(ctx context.Context, cluster string) (*talosclient.Client, func(), error) {
	if cluster == "" {
		if b.direct == nil {
			return nil, nil, errors.New("direct Talos access is disabled; set cluster to route through Omni")
		}

		return b.direct, func() {}, nil
	}

	if b.omni == nil {
		return nil, nil, errors.New("Omni access is disabled; omit cluster to use talosconfig directly") //nolint:staticcheck
	}

	cfg, err := b.omni.config(ctx)
	if err != nil {
		return nil, nil, err
	}

	// ponytail: new gRPC connection per call, cache per cluster if latency matters
	c, err := talosclient.New(ctx,
		talosclient.WithConfig(cfg),
		talosclient.WithServiceAccount(b.omni.key),
		talosclient.WithCluster(cluster),
	)
	if err != nil {
		return nil, nil, err
	}

	return c, func() { c.Close() }, nil //nolint:errcheck
}

type talosTarget struct {
	Cluster string   `json:"cluster,omitempty" jsonschema:"Omni cluster name: routes the request through Omni; omit to use the local talosconfig directly"`
	Nodes   []string `json:"nodes" jsonschema:"target nodes: IPs/hostnames, or Omni machine IDs/node names when routed through Omni"`
}

type talosGetInput struct {
	tools.ResourceQuery
	talosTarget
}

type talosLogsInput struct {
	Service string `json:"service" jsonschema:"service ID, e.g. kubelet, etcd, apid, machined"`
	talosTarget
	TailLines int32 `json:"tail_lines,omitempty" jsonschema:"number of lines from the end, default 200"`
}

type talosDmesgInput struct {
	talosTarget
	TailLines int `json:"tail_lines,omitempty" jsonschema:"number of lines from the end, default 200"`
}

// talosTool registers a read-only Talos tool which runs fn for each target node.
func talosTool[In interface{ target() talosTarget }](
	server *mcp.Server, b backends, name, description string,
	fn func(ctx context.Context, c *talosclient.Client, in In) (string, error),
) {
	mcp.AddTool(server, &mcp.Tool{
		Name:        name,
		Description: description,
		Annotations: &mcp.ToolAnnotations{ReadOnlyHint: true},
	}, func(ctx context.Context, _ *mcp.CallToolRequest, in In) (*mcp.CallToolResult, any, error) {
		t := in.target()

		if len(t.Nodes) == 0 {
			return nil, nil, errors.New("at least one node is required")
		}

		c, release, err := b.client(ctx, t.Cluster)
		if err != nil {
			return nil, nil, err
		}

		defer release()

		var sb strings.Builder

		for _, node := range t.Nodes {
			out, err := fn(talosclient.WithNode(ctx, node), c, in)
			if err != nil {
				out = "error: " + err.Error() + "\n"
			}

			fmt.Fprintf(&sb, "--- node %s ---\n%s", node, out)
		}

		return tools.Text(sb.String()), nil, nil
	})
}

func (t talosTarget) target() talosTarget { return t }

// Register adds the Talos tools to the server.
func Register(server *mcp.Server, routes Backends) {
	b := backends{direct: routes.Direct}

	if routes.Omni != nil {
		// same service account as the Omni client, see omni.NewClient
		b.omni = &omniTalos{c: routes.Omni, key: os.Getenv("OMNI_SERVICE_ACCOUNT_KEY")}
	}

	talosTool(server, b, "talos_get",
		"Get or list Talos COSI resources as YAML on each node, equivalent to `talosctl get`. Use type 'rd' to discover available types.",
		func(ctx context.Context, c *talosclient.Client, in talosGetInput) (string, error) {
			return tools.GetResources(ctx, c.COSI, in.ResourceQuery)
		})

	talosTool(server, b, "talos_logs",
		"Get service logs from each node, equivalent to `talosctl logs`.",
		func(ctx context.Context, c *talosclient.Client, in talosLogsInput) (string, error) {
			tail := in.TailLines
			if tail <= 0 {
				tail = 200
			}

			stream, err := c.Logs(ctx, constants.SystemContainerdNamespace, common.ContainerDriver_CONTAINERD, in.Service, false, tail)
			if err != nil {
				return "", err
			}

			return readStream(stream.Recv)
		})

	talosTool(server, b, "talos_dmesg",
		"Get kernel log from each node, equivalent to `talosctl dmesg`.",
		func(ctx context.Context, c *talosclient.Client, in talosDmesgInput) (string, error) {
			stream, err := c.Dmesg(ctx, false, false)
			if err != nil {
				return "", err
			}

			out, err := readStream(stream.Recv)
			if err != nil {
				return "", err
			}

			tail := in.TailLines
			if tail <= 0 {
				tail = 200
			}

			lines := strings.SplitAfter(out, "\n")

			return strings.Join(lines[max(0, len(lines)-tail):], ""), nil
		})

	talosTool(server, b, "talos_version",
		"Get Talos version information from each node, equivalent to `talosctl version`.",
		func(ctx context.Context, c *talosclient.Client, _ talosTarget) (string, error) {
			resp, err := c.Version(ctx)
			if err != nil {
				return "", err
			}

			out, err := protojson.Marshal(resp)

			return string(out) + "\n", err
		})
}

// readStream drains a Talos data stream into a string.
func readStream(recv func() (*common.Data, error)) (string, error) {
	var buf bytes.Buffer

	for {
		msg, err := recv()
		if errors.Is(err, io.EOF) {
			return buf.String(), nil
		}

		if err != nil {
			return buf.String(), err
		}

		buf.Write(msg.GetBytes())
	}
}

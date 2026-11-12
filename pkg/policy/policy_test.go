// This Source Code Form is subject to the terms of the Mozilla Public
// License, v. 2.0. If a copy of the MPL was not distributed with this
// file, You can obtain one at http://mozilla.org/MPL/2.0/.

package policy_test

import (
	"context"
	"slices"
	"strings"
	"testing"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/siderolabs/mcp-server/pkg/policy"
)

const custom = `
rules:
  - tools: [omni_get]
    when: args.type == "secrets"
    action: deny
  - tools: ["talos_*"]
    when: has(args.cluster) && args.cluster in ["dev", "staging"]
    action: allow
  - tools: ["talos_*"]
    when: '!has(args.cluster) && args.nodes.all(n, n.startsWith("10.5."))'
    action: allow
  - tools: [talos_logs]
    when: args.tail_lines > 1000
    action: deny
  - tools: ["omni_*"]
    action: allow
`

func TestAllowed(t *testing.T) {
	def, err := policy.Load("")
	if err != nil {
		t.Fatal(err)
	}

	cus, err := policy.Parse([]byte(custom))
	if err != nil {
		t.Fatal(err)
	}

	for _, tc := range []struct {
		policy  *policy.Policy
		args    map[string]any
		name    string
		tool    string
		allowed bool
		visible bool
	}{
		{name: "default kubeconfig", policy: def, tool: "omni_kubeconfig", visible: false},
		{name: "default sync", policy: def, tool: "omni_template_sync", visible: true},
		{name: "default sync dry run", policy: def, tool: "omni_template_sync", args: map[string]any{"dry_run": true}, allowed: true, visible: true},
		{name: "default sync dry run false", policy: def, tool: "omni_template_sync", args: map[string]any{"dry_run": false}, visible: true},
		{name: "default get", policy: def, tool: "omni_get", allowed: true, visible: true},
		{name: "default talos", policy: def, tool: "talos_dmesg", allowed: true, visible: true},
		{name: "default factory", policy: def, tool: "factory_schematic_create", allowed: true, visible: true},
		{name: "default unknown", policy: def, tool: "something_else"},

		{name: "secrets denied", policy: cus, tool: "omni_get", args: map[string]any{"type": "secrets"}, visible: true},
		{name: "clusters allowed", policy: cus, tool: "omni_get", args: map[string]any{"type": "clusters"}, allowed: true, visible: true},
		// a deny condition failing to evaluate (no args.type) denies
		{name: "missing type fails closed", policy: cus, tool: "omni_get", visible: true},
		{name: "talos dev cluster", policy: cus, tool: "talos_version", args: map[string]any{"cluster": "dev", "nodes": []any{"a"}}, allowed: true, visible: true},
		{name: "talos prod cluster", policy: cus, tool: "talos_version", args: map[string]any{"cluster": "prod", "nodes": []any{"a"}}, visible: true},
		{name: "talos direct allowed nodes", policy: cus, tool: "talos_version", args: map[string]any{"nodes": []any{"10.5.0.2"}}, allowed: true, visible: true},
		{name: "talos direct other nodes", policy: cus, tool: "talos_version", args: map[string]any{"nodes": []any{"10.5.0.2", "192.168.1.1"}}, visible: true},
		{
			name: "logs not matched by deny after allow", policy: cus, tool: "talos_logs",
			args:    map[string]any{"cluster": "dev", "nodes": []any{"a"}, "tail_lines": 5000.0},
			allowed: true, visible: true,
		},
		{name: "factory denied by default", policy: cus, tool: "factory_versions"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if tc.args == nil {
				tc.args = map[string]any{}
			}

			if allowed, reason := tc.policy.Allowed(tc.tool, tc.args); allowed != tc.allowed {
				t.Errorf("Allowed = %v (%s), want %v", allowed, reason, tc.allowed)
			}

			if visible := tc.policy.Visible(tc.tool); visible != tc.visible {
				t.Errorf("Visible = %v, want %v", visible, tc.visible)
			}
		})
	}
}

func TestParseErrors(t *testing.T) {
	for name, in := range map[string]string{
		"unknown field":   "rules:\n  - tools: [a]\n    action: allow\n    unknown: 1\n",
		"bad action":      "rules:\n  - tools: [a]\n    action: maybe\n",
		"no tools":        "rules:\n  - action: allow\n",
		"bad glob":        "rules:\n  - tools: ['[']\n    action: allow\n",
		"bad cel":         "rules:\n  - tools: [a]\n    when: 'args.'\n    action: allow\n",
		"non-bool cel":    "rules:\n  - tools: [a]\n    when: '\"x\"'\n    action: allow\n",
		"unknown var cel": "rules:\n  - tools: [a]\n    when: 'foo == 1'\n    action: allow\n",
	} {
		t.Run(name, func(t *testing.T) {
			if _, err := policy.Parse([]byte(in)); err == nil {
				t.Fatal("expected error")
			}
		})
	}
}

func TestMiddleware(t *testing.T) {
	ctx := context.Background()

	pol, err := policy.Load("")
	if err != nil {
		t.Fatal(err)
	}

	server := mcp.NewServer(&mcp.Implementation{Name: "test", Version: "0"}, nil)
	server.AddReceivingMiddleware(pol.Middleware)

	type in struct {
		DryRun bool `json:"dry_run,omitempty"`
	}

	for _, name := range []string{"omni_kubeconfig", "omni_template_sync", "omni_get"} {
		mcp.AddTool(server, &mcp.Tool{Name: name}, func(context.Context, *mcp.CallToolRequest, in) (*mcp.CallToolResult, any, error) {
			return &mcp.CallToolResult{Content: []mcp.Content{&mcp.TextContent{Text: "ok"}}}, nil, nil
		})
	}

	st, ct := mcp.NewInMemoryTransports()

	if _, err = server.Connect(ctx, st, nil); err != nil {
		t.Fatal(err)
	}

	session, err := mcp.NewClient(&mcp.Implementation{Name: "test", Version: "0"}, nil).Connect(ctx, ct, nil)
	if err != nil {
		t.Fatal(err)
	}

	defer session.Close() //nolint:errcheck

	list, err := session.ListTools(ctx, nil)
	if err != nil {
		t.Fatal(err)
	}

	names := make([]string, 0, len(list.Tools))
	for _, tool := range list.Tools {
		names = append(names, tool.Name)
	}

	slices.Sort(names)

	if !slices.Equal(names, []string{"omni_get", "omni_template_sync"}) {
		t.Fatalf("unexpected tools: %v", names)
	}

	for _, tc := range []struct {
		args   map[string]any
		tool   string
		denied bool
	}{
		{tool: "omni_get"},
		{tool: "omni_kubeconfig", denied: true},
		{tool: "omni_template_sync", denied: true},
		{tool: "omni_template_sync", args: map[string]any{"dry_run": true}},
	} {
		res, err := session.CallTool(ctx, &mcp.CallToolParams{Name: tc.tool, Arguments: tc.args})
		if err != nil {
			t.Fatal(err)
		}

		content, _ := res.Content[0].(*mcp.TextContent) //nolint:errcheck
		text := content.Text

		if res.IsError != tc.denied || tc.denied != strings.Contains(text, "denied by policy") {
			t.Errorf("%s %v: IsError=%v %q, want denied=%v", tc.tool, tc.args, res.IsError, text, tc.denied)
		}
	}
}

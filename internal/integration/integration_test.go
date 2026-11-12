// This Source Code Form is subject to the terms of the Mozilla Public
// License, v. 2.0. If a copy of the MPL was not distributed with this
// file, You can obtain one at http://mozilla.org/MPL/2.0/.

//go:build integration

// Package integration_test runs the siderolabs-mcp-server binary over stdio against a throwaway
// Omni with real Talos machines, see hack/test/e2e-qemu.sh.
package integration_test

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"
	yaml "go.yaml.in/yaml/v4"
)

const (
	clusterName  = "mcp-e2e"
	readyTimeout = 20 * time.Minute
)

type server struct {
	session *mcp.ClientSession
}

// startServer runs the server binary with the given args and extra environment.
func startServer(t *testing.T, env []string, args ...string) *server {
	t.Helper()

	bin := os.Getenv("E2E_SERVER_BINARY")
	if bin == "" {
		t.Fatal("E2E_SERVER_BINARY is not set")
	}

	cmd := exec.Command(bin, args...)
	// no talosconfig unless the test provides one
	cmd.Env = append(os.Environ(), "HOME="+t.TempDir(), "TALOSCONFIG="+filepath.Join(t.TempDir(), "none"))
	cmd.Env = append(cmd.Env, env...)
	cmd.Stderr = os.Stderr

	client := mcp.NewClient(&mcp.Implementation{Name: "e2e", Version: "0"}, nil)

	session, err := client.Connect(t.Context(), &mcp.CommandTransport{Command: cmd}, nil)
	if err != nil {
		t.Fatalf("failed to connect to server: %v", err)
	}

	t.Cleanup(func() { session.Close() }) //nolint:errcheck

	return &server{session: session}
}

// bearer adds a bearer token to requests.
type bearer struct {
	next  http.RoundTripper
	token string
}

func (b bearer) RoundTrip(req *http.Request) (*http.Response, error) {
	req = req.Clone(req.Context())
	req.Header.Set("Authorization", "Bearer "+b.token)

	return b.next.RoundTrip(req)
}

// startHTTPServer runs `serve` on a free loopback port with the token and returns a connected client and the endpoint.
func startHTTPServer(t *testing.T, token string, args ...string) (*server, string) {
	t.Helper()

	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}

	addr := l.Addr().String()
	l.Close() //nolint:errcheck

	cmd := exec.Command(os.Getenv("E2E_SERVER_BINARY"), append([]string{"serve", "--listen", addr}, args...)...)
	cmd.Env = append(os.Environ(), "HOME="+t.TempDir(), "TALOSCONFIG="+filepath.Join(t.TempDir(), "none"), "MCP_AUTH_TOKEN="+token)
	cmd.Stderr = os.Stderr

	if err = cmd.Start(); err != nil {
		t.Fatal(err)
	}

	t.Cleanup(func() {
		cmd.Process.Signal(os.Interrupt) //nolint:errcheck
		cmd.Wait()                       //nolint:errcheck
	})

	endpoint := "http://" + addr + "/mcp"
	client := mcp.NewClient(&mcp.Implementation{Name: "e2e", Version: "0"}, nil)

	var session *mcp.ClientSession

	eventually(t, 30*time.Second, func() error {
		session, err = client.Connect(t.Context(), &mcp.StreamableClientTransport{
			Endpoint:   endpoint,
			HTTPClient: &http.Client{Transport: bearer{next: http.DefaultTransport, token: token}},
		}, nil)

		return err
	})

	t.Cleanup(func() { session.Close() }) //nolint:errcheck

	return &server{session: session}, endpoint
}

// policyArgs writes a policy file and returns the server args using it.
func policyArgs(t *testing.T, policy string) []string {
	t.Helper()

	path := filepath.Join(t.TempDir(), "policy.yaml")
	if err := os.WriteFile(path, []byte(policy), 0o600); err != nil {
		t.Fatal(err)
	}

	return []string{"--policy", path}
}

func (s *server) tools(t *testing.T) []string {
	t.Helper()

	list, err := s.session.ListTools(t.Context(), nil)
	if err != nil {
		t.Fatal(err)
	}

	names := make([]string, 0, len(list.Tools))
	for _, tool := range list.Tools {
		names = append(names, tool.Name)
	}

	slices.Sort(names)

	return names
}

func (s *server) call(ctx context.Context, name string, args map[string]any) (string, error) {
	res, err := s.session.CallTool(ctx, &mcp.CallToolParams{Name: name, Arguments: args})
	if err != nil {
		return "", err
	}

	var sb strings.Builder

	for _, c := range res.Content {
		if tc, ok := c.(*mcp.TextContent); ok {
			sb.WriteString(tc.Text)
		}
	}

	if res.IsError {
		return "", errors.New(sb.String())
	}

	return sb.String(), nil
}

// mustCall calls a tool, failing the test on error. Talos tools report per-node errors inline.
func (s *server) mustCall(t *testing.T, name string, args map[string]any) string {
	t.Helper()

	out, err := s.call(t.Context(), name, args)
	if err != nil {
		t.Fatalf("%s: %v", name, err)
	}

	return out
}

type res struct {
	Metadata struct {
		ID string `yaml:"id"`
	} `yaml:"metadata"`
	Spec map[string]any `yaml:"spec"`
}

func decode(t *testing.T, out string) []res {
	t.Helper()

	var items []res

	dec := yaml.NewDecoder(strings.NewReader(out))

	for {
		var r res

		if err := dec.Decode(&r); errors.Is(err, io.EOF) {
			return items
		} else if err != nil {
			t.Fatalf("failed to decode %q: %v", out, err)
		}

		items = append(items, r)
	}
}

// eventually polls fn until it returns nil or the timeout expires.
func eventually(t *testing.T, timeout time.Duration, fn func() error) {
	t.Helper()

	deadline := time.Now().Add(timeout)

	for {
		err := fn()
		if err == nil {
			return
		}

		if time.Now().After(deadline) {
			t.Fatalf("timed out after %s: %v", timeout, err)
		}

		time.Sleep(5 * time.Second)
	}
}

func TestE2E(t *testing.T) {
	talosVersion := os.Getenv("E2E_TALOS_VERSION")

	machineCount, err := strconv.Atoi(os.Getenv("E2E_MACHINE_COUNT"))
	if err != nil || machineCount < 1 {
		t.Fatalf("invalid E2E_MACHINE_COUNT: %v", err)
	}

	s := startServer(t, nil, policyArgs(t, "rules:\n  - tools: [\"*\"]\n    action: allow\n")...)

	var machines []string

	t.Run("machines", func(t *testing.T) {
		eventually(t, 10*time.Minute, func() error {
			machines = nil

			out, err := s.call(t.Context(), "omni_get", map[string]any{"type": "machinestatus"})
			if err != nil {
				return err
			}

			for _, m := range decode(t, out) {
				if m.Spec["connected"] == true {
					machines = append(machines, m.Metadata.ID)
				}
			}

			if len(machines) < machineCount {
				return fmt.Errorf("%d/%d machines connected", len(machines), machineCount)
			}

			return nil
		})
	})

	if len(machines) == 0 {
		t.FailNow()
	}

	// ponytail: single-node control plane, enough to exercise every tool; scale when tools need it
	tmpl := s.clusterTemplate(t, talosVersion, machines[0])

	t.Run("template-dry-run", func(t *testing.T) {
		out := s.mustCall(t, "omni_template_sync", map[string]any{"template": tmpl, "dry_run": true})
		if !strings.Contains(out, clusterName) {
			t.Fatalf("dry run did not report the cluster: %s", out)
		}

		if _, err := s.call(t.Context(), "omni_get", map[string]any{"type": "cluster", "id": clusterName}); err == nil {
			t.Fatal("dry run created the cluster")
		}
	})

	t.Run("template-sync", func(t *testing.T) {
		s.mustCall(t, "omni_template_sync", map[string]any{"template": tmpl})

		eventually(t, readyTimeout, func() error {
			out, err := s.call(t.Context(), "omni_get", map[string]any{"type": "clusterstatus", "id": clusterName})
			if err != nil {
				return err
			}

			if items := decode(t, out); len(items) != 1 || items[0].Spec["ready"] != true || items[0].Spec["kubernetesapiready"] != true {
				return fmt.Errorf("cluster not ready: %s", out)
			}

			return nil
		})
	})

	if t.Failed() {
		t.FailNow()
	}

	t.Run("configs", func(t *testing.T) {
		talosconfig := s.mustCall(t, "omni_talosconfig", map[string]any{"cluster": clusterName})
		if !strings.Contains(talosconfig, "contexts:") {
			t.Fatalf("unexpected talosconfig: %s", talosconfig)
		}

		kubeconfig := s.mustCall(t, "omni_kubeconfig", map[string]any{"cluster": clusterName, "ttl": "10m"})
		if !strings.Contains(kubeconfig, "clusters:") || !strings.Contains(kubeconfig, "token:") {
			t.Fatalf("unexpected kubeconfig: %s", kubeconfig)
		}
	})

	t.Run("serve", func(t *testing.T) {
		s, endpoint := startHTTPServer(t, "e2e-token")

		resp, err := http.Post(endpoint, "application/json", strings.NewReader(`{"jsonrpc":"2.0","id":1,"method":"tools/list"}`)) //nolint:noctx
		if err != nil {
			t.Fatal(err)
		}

		resp.Body.Close() //nolint:errcheck

		if resp.StatusCode != http.StatusUnauthorized {
			t.Fatalf("unauthenticated request: got %s, want 401", resp.Status)
		}

		if out := s.mustCall(t, "omni_get", map[string]any{"type": "cluster", "id": clusterName}); !strings.Contains(out, clusterName) {
			t.Fatalf("unexpected omni_get output: %s", out)
		}

		// default policy applies over HTTP as well
		if _, err := s.call(t.Context(), "omni_kubeconfig", map[string]any{"cluster": clusterName}); err == nil || !strings.Contains(err.Error(), "denied by policy") {
			t.Fatalf("expected policy denial, got %v", err)
		}
	})

	t.Run("default-policy", func(t *testing.T) {
		def := startServer(t, nil)

		if slices.Contains(def.tools(t), "omni_kubeconfig") {
			t.Fatal("omni_kubeconfig must be hidden by the default policy")
		}

		def.mustCall(t, "omni_get", map[string]any{"type": "cluster", "id": clusterName})
		def.mustCall(t, "omni_template_sync", map[string]any{"template": tmpl, "dry_run": true})

		for tool, args := range map[string]map[string]any{
			"omni_template_sync": {"template": tmpl},
			"omni_kubeconfig":    {"cluster": clusterName},
		} {
			if _, err := def.call(t.Context(), tool, args); err == nil || !strings.Contains(err.Error(), "denied by policy") {
				t.Fatalf("%s: expected policy denial, got %v", tool, err)
			}
		}
	})
}

func (s *server) clusterTemplate(t *testing.T, talosVersion, machine string) string {
	t.Helper()

	items := decode(t, s.mustCall(t, "omni_get", map[string]any{"type": "talosversion", "id": strings.TrimPrefix(talosVersion, "v")}))
	if len(items) != 1 {
		t.Fatalf("Talos version %s not known to Omni", talosVersion)
	}

	k8s, _ := items[0].Spec["compatiblekubernetesversions"].([]any)
	if len(k8s) == 0 {
		t.Fatalf("no compatible Kubernetes versions for %s", talosVersion)
	}

	return fmt.Sprintf(`kind: Cluster
name: %s
kubernetes:
  version: v%s
talos:
  version: %s
---
kind: ControlPlane
machines:
  - %s
`, clusterName, k8s[len(k8s)-1], talosVersion, machine)
}

# siderolabs-mcp-server

[MCP](https://modelcontextprotocol.io) server exposing Sidero Labs products ([Omni](https://github.com/siderolabs/omni), [Talos](https://github.com/siderolabs/talos), Image Factory) to agents over stdio or HTTP.

## Backends

| Backend | Enabled when |
| ------- | ------------ |
| Omni | `OMNI_ENDPOINT` and `OMNI_SERVICE_ACCOUNT_KEY` set |
| Talos (direct) | usable talosconfig (`TALOSCONFIG` or `~/.talos/config`; `--talos-context` picks a context) |
| Image Factory | always (`--image-factory-url`, default `https://factory.talos.dev`) |

What an agent may actually call is decided by the [policy](#policy).

## Tools

| Tool | Description |
| ---- | ----------- |
| `omni_get` | Get/list any Omni COSI resource as YAML (like `omnictl get`) |
| `omni_talosconfig` | Cluster talosconfig |
| `omni_kubeconfig` | Service-account kubeconfig for a cluster |
| `omni_template_sync` | Apply a cluster template, with dry-run (like `omnictl cluster template sync`) |
| `talos_get` | Get/list Talos COSI resources (like `talosctl get`) |
| `talos_logs` | Service logs (like `talosctl logs`) |
| `talos_dmesg` | Kernel log (like `talosctl dmesg`) |
| `talos_version` | Talos version (like `talosctl version`) |
| `factory_versions` | Talos versions available in Image Factory |
| `factory_extensions` | System extensions and overlays for a Talos version |
| `factory_schematic_get` | Get a schematic by ID |
| `factory_schematic_create` | Upload a schematic, returns ID and image references |

Talos tools take `nodes` and an optional `cluster`: with `cluster` the request is routed through Omni, without it the local talosconfig is used.

## Policy

`--policy <file>` restricts tool calls.
Rules are evaluated in order: the first rule whose `tools` glob matches and whose optional `when` [CEL](https://cel.dev) condition holds decides.
Calls matching no rule are denied, and tools no rule can ever allow are hidden from the tool list.
Conditions see `tool` (string) and `args` (the call arguments).
A condition that fails to evaluate, like one reading a missing argument (guard with `has(args.x)`), never allows a call, and denies it for a `deny` rule.

```yaml
rules:
  - tools: [omni_kubeconfig]
    action: deny
  - tools: [omni_get]
    when: has(args.type) && args.type in ["secrets", "identities"]
    action: deny
  - tools: [omni_template_sync]
    when: has(args.dry_run) && args.dry_run
    action: allow
  # Talos only through Omni, and only for these clusters
  - tools: ["talos_*"]
    when: has(args.cluster) && args.cluster in ["dev", "staging"]
    action: allow
  - tools: [omni_get, omni_talosconfig, "factory_*"]
    action: allow
```

Without `--policy` the built-in [read-only policy](pkg/policy/default.yaml) applies:
everything except `omni_kubeconfig` (which mints cluster-admin credentials), and `omni_template_sync` only as a dry run.

## Usage

Create an Omni service account (`omnictl serviceaccount create mcp`) and pass its credentials to the server
through `OMNI_ENDPOINT` and `OMNI_SERVICE_ACCOUNT_KEY`.
Template file references (like patch files) are resolved relative to, and confined to, the server's working directory.

### stdio

Without a subcommand the server speaks MCP over stdio; register it with your MCP client:

```json
{
  "mcpServers": {
    "siderolabs": {
      "command": "siderolabs-mcp-server",
      "args": ["--policy", "/path/to/policy.yaml"],
      "env": {
        "OMNI_ENDPOINT": "https://<account>.omni.siderolabs.io",
        "OMNI_SERVICE_ACCOUNT_KEY": "<key>"
      }
    }
  }
}
```

### HTTP

`siderolabs-mcp-server serve` exposes the same tools, under the same policy, over the MCP streamable HTTP transport at `/mcp`:

```sh
export OMNI_ENDPOINT=https://<account>.omni.siderolabs.io OMNI_SERVICE_ACCOUNT_KEY=<key>
export MCP_AUTH_TOKEN=$(openssl rand -hex 32)

siderolabs-mcp-server serve --listen 0.0.0.0:8080 --policy policy.yaml
```

Clients authenticate with `Authorization: Bearer $MCP_AUTH_TOKEN`:

```json
{
  "mcpServers": {
    "siderolabs": {
      "type": "http",
      "url": "http://<host>:8080/mcp",
      "headers": {
        "Authorization": "Bearer <token>"
      }
    }
  }
}
```

The server refuses non-loopback listen addresses without a token (it acts with your Omni and Talos credentials);
the default is `127.0.0.1:8080`, where the token is optional.

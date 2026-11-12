# siderolabs-mcp-server

[MCP](https://modelcontextprotocol.io) server exposing Sidero Labs products to agents over stdio or HTTP.

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

### stdio

Without a subcommand the server speaks MCP over stdio; register it with your MCP client:

```json
{
  "mcpServers": {
    "siderolabs": {
      "command": "siderolabs-mcp-server",
      "args": ["--policy", "/path/to/policy.yaml"]
    }
  }
}
```

### HTTP

`siderolabs-mcp-server serve` exposes the same tools, under the same policy, over the MCP streamable HTTP transport at `/mcp`:

```sh
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

The server refuses non-loopback listen addresses without a token (it acts with the configured credentials);
the default is `127.0.0.1:8080`, where the token is optional.

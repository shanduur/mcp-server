// This Source Code Form is subject to the terms of the Mozilla Public
// License, v. 2.0. If a copy of the MPL was not distributed with this
// file, You can obtain one at http://mozilla.org/MPL/2.0/.

// Package policy restricts which tools can be called, and with which arguments.
//
// A policy is an ordered list of rules. The first rule whose tool glob matches and whose
// optional CEL condition holds decides; when no rule matches, the call is denied.
package policy

import (
	"bytes"
	"context"
	_ "embed"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path"

	"cel.dev/cel-go/cel"
	"github.com/modelcontextprotocol/go-sdk/mcp"
	yaml "go.yaml.in/yaml/v4"
)

// Default is the policy used when no policy file is given: read-only access.
//
//go:embed default.yaml
var Default []byte

// Action is the decision of a rule.
type Action string

// Actions.
const (
	Allow Action = "allow"
	Deny  Action = "deny"
)

// Rule matches tool calls by tool name glob and an optional CEL condition.
type Rule struct {
	prg cel.Program

	// When is a CEL expression over `tool` (string) and `args` (map); empty always matches.
	When   string   `yaml:"when"`
	Action Action   `yaml:"action"`
	Tools  []string `yaml:"tools"`
}

// Policy is an ordered list of rules, first match wins, default deny.
type Policy struct {
	Rules []Rule `yaml:"rules"`
}

// Load reads a policy file, or returns the default policy when path is empty.
func Load(path string) (*Policy, error) {
	if path == "" {
		return Parse(Default)
	}

	data, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}

	return Parse(data)
}

// Parse parses and compiles a policy.
func Parse(data []byte) (*Policy, error) {
	dec := yaml.NewDecoder(bytes.NewReader(data))
	dec.KnownFields(true)

	var p Policy

	if err := dec.Decode(&p); err != nil {
		return nil, fmt.Errorf("invalid policy: %w", err)
	}

	env, err := cel.NewEnv(
		cel.Variable("tool", cel.StringType),
		cel.Variable("args", cel.MapType(cel.StringType, cel.DynType)),
	)
	if err != nil {
		return nil, err
	}

	for i := range p.Rules {
		r := &p.Rules[i]

		if r.Action != Allow && r.Action != Deny {
			return nil, fmt.Errorf("rule %d: action must be %q or %q", i, Allow, Deny)
		}

		if len(r.Tools) == 0 {
			return nil, fmt.Errorf("rule %d: tools must not be empty", i)
		}

		for _, glob := range r.Tools {
			if _, err = path.Match(glob, ""); err != nil {
				return nil, fmt.Errorf("rule %d: invalid tool glob %q: %w", i, glob, err)
			}
		}

		if r.When == "" {
			continue
		}

		ast, iss := env.Compile(r.When)
		if iss.Err() != nil {
			return nil, fmt.Errorf("rule %d: %w", i, iss.Err())
		}

		if ast.OutputType() != cel.BoolType {
			return nil, fmt.Errorf("rule %d: condition must be boolean, got %s", i, ast.OutputType())
		}

		if r.prg, err = env.Program(ast); err != nil {
			return nil, fmt.Errorf("rule %d: %w", i, err)
		}
	}

	return &p, nil
}

func (r *Rule) matchesTool(tool string) bool {
	for _, glob := range r.Tools {
		if ok, _ := path.Match(glob, tool); ok { //nolint:errcheck // validated in Parse
			return true
		}
	}

	return false
}

// Allowed decides a tool call. Conditions failing to evaluate (e.g. a missing argument) fail closed:
// an allow rule does not match, a deny rule denies.
func (p *Policy) Allowed(tool string, args map[string]any) (bool, string) {
	for i := range p.Rules {
		r := &p.Rules[i]

		if !r.matchesTool(tool) {
			continue
		}

		if r.prg != nil {
			out, _, err := r.prg.Eval(map[string]any{"tool": tool, "args": args})

			switch {
			case err != nil && r.Action == Deny:
				return false, fmt.Sprintf("rule %d condition failed: %v", i, err)
			case err != nil, out.Value() != true:
				continue
			}
		}

		return r.Action == Allow, fmt.Sprintf("rule %d", i)
	}

	return false, "no rule matched"
}

// Visible reports whether some call to the tool could be allowed.
func (p *Policy) Visible(tool string) bool {
	for i := range p.Rules {
		r := &p.Rules[i]

		if !r.matchesTool(tool) {
			continue
		}

		if r.Action == Allow {
			return true
		}

		if r.prg == nil {
			return false
		}
	}

	return false
}

// Middleware enforces the policy on tool calls and hides tools which can never be called.
func (p *Policy) Middleware(next mcp.MethodHandler) mcp.MethodHandler {
	return func(ctx context.Context, method string, req mcp.Request) (mcp.Result, error) {
		switch method {
		case "tools/call":
			params, ok := req.GetParams().(*mcp.CallToolParamsRaw)
			if !ok {
				return nil, errors.New("unexpected tools/call params")
			}

			args := map[string]any{}

			if len(params.Arguments) > 0 && string(params.Arguments) != "null" {
				if err := json.Unmarshal(params.Arguments, &args); err != nil {
					return nil, fmt.Errorf("invalid arguments: %w", err)
				}
			}

			if ok, reason := p.Allowed(params.Name, args); !ok {
				return &mcp.CallToolResult{
					IsError: true,
					Content: []mcp.Content{&mcp.TextContent{Text: fmt.Sprintf("tool %q denied by policy (%s)", params.Name, reason)}},
				}, nil
			}
		case "tools/list":
			res, err := next(ctx, method, req)
			if list, ok := res.(*mcp.ListToolsResult); ok && err == nil {
				visible := list.Tools[:0]

				for _, t := range list.Tools {
					if p.Visible(t.Name) {
						visible = append(visible, t)
					}
				}

				list.Tools = visible
			}

			return res, err
		}

		return next(ctx, method, req)
	}
}

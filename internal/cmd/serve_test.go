// This Source Code Form is subject to the terms of the Mozilla Public
// License, v. 2.0. If a copy of the MPL was not distributed with this
// file, You can obtain one at http://mozilla.org/MPL/2.0/.

package cmd_test

import (
	"testing"

	"github.com/siderolabs/mcp-server/internal/cmd"
)

func TestCheckListen(t *testing.T) {
	for _, tc := range []struct {
		addr, token string
		ok          bool
	}{
		{addr: "127.0.0.1:8080", ok: true},
		{addr: "[::1]:8080", ok: true},
		{addr: "localhost:8080", ok: true},
		{addr: ":8080"},
		{addr: "0.0.0.0:8080"},
		{addr: "10.0.0.1:8080"},
		{addr: "0.0.0.0:8080", token: "secret", ok: true},
		{addr: "bad", token: "secret"},
	} {
		if err := cmd.CheckListen(tc.addr, tc.token); (err == nil) != tc.ok {
			t.Errorf("CheckListen(%q, %q) = %v, want ok=%v", tc.addr, tc.token, err, tc.ok)
		}
	}
}

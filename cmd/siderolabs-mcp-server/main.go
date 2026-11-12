// This Source Code Form is subject to the terms of the Mozilla Public
// License, v. 2.0. If a copy of the MPL was not distributed with this
// file, You can obtain one at http://mozilla.org/MPL/2.0/.

// Package main is the siderolabs-mcp-server entrypoint.
package main

import (
	"log"

	"github.com/siderolabs/mcp-server/internal/cmd"
)

// version is set at build time.
var version = "dev"

func main() {
	if err := cmd.Execute(version); err != nil {
		log.Fatal(err)
	}
}

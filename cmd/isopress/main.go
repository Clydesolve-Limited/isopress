// Command isopress is the entry point for the isopress CLI.
//
// Since T0.5 it wires the full command tree of DESIGN_SPEC 1.1 through
// cli.Execute, which maps command results to the exit-code scheme of
// DESIGN_SPEC 10.2 (errors print to stderr; stdout carries results only,
// FR-046).
package main

import (
	"os"

	"isopress/internal/cli"
)

func main() {
	os.Exit(cli.Execute())
}

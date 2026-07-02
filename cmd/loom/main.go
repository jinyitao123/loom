// Command loom is a thin CLI wrapper around the Loom engine that speaks the
// weave daemon's spawn-harness protocol: it reads a prompt JSON on stdin, runs
// an agent turn, and emits a stdout-json (NDJSON) event stream the daemon's
// LoomBackend parses (see weave src/cli/daemon/agent/loom.ts and
// plans/loom-cli-design.md).
//
// Usage:
//
//	loom run --format stream-json --cwd <dir> --bypass-permissions \
//	         [--model deepseek-chat] [--resume <sessionId>] < prompt.json
//	loom version
package main

import (
	"fmt"
	"os"
)

// version is injected at release time via -ldflags "-X main.version=...".
// Defaults to "dev" for local builds.
var version = "dev"

func main() {
	if len(os.Args) < 2 {
		fmt.Fprintln(os.Stderr, "usage: loom <command>\n\ncommands:\n  run        run one agent turn (reads prompt JSON from stdin)\n  version    print version")
		os.Exit(2)
	}
	switch os.Args[1] {
	case "run":
		os.Exit(runCmd(os.Args[2:]))
	case "version", "--version", "-v":
		fmt.Printf("loom %s\n", version)
	default:
		fmt.Fprintf(os.Stderr, "loom: unknown command %q\n", os.Args[1])
		os.Exit(2)
	}
}

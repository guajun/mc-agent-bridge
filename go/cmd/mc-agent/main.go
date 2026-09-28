// Command mc-agent is the single Go binary: short CLI commands plus a daemon.
// There is no MCP server and no Python runtime dependency.
package main

import (
	"os"

	"github.com/guajun/mc-agent-bridge/internal/cli"
)

func main() {
	os.Exit(cli.Main(os.Args[1:]))
}

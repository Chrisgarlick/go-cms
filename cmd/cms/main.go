// Command cms is the single binary: server, worker, CLI and site tooling.
package main

import (
	"os"

	"cms/internal/cli"
)

func main() { os.Exit(cli.Execute()) }

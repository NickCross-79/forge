// Command forge is a local-first CI/CD runner.
//
// Everything it does happens on this machine: pipelines are defined in YAML,
// executed as child processes or in containers, and recorded in a local SQLite
// database. There is no server to deploy and no service to pay for.
package main

import (
	"context"
	"os"

	"github.com/nickcross-79/forge/internal/cli"
)

func main() {
	os.Exit(cli.Execute(context.Background(), os.Args[1:], os.Stdout, os.Stderr))
}

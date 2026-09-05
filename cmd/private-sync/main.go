// Command private-sync finds configuration/secret files in your projects,
// stores them encrypted in a vault and keeps that vault in sync across machines.
package main

import (
	"os"

	"github.com/sanbiv/private-sync/internal/cli"
)

func main() {
	os.Exit(cli.Main(os.Args[1:]))
}

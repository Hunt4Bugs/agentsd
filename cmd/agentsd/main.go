// Command agentsd runs and supervises AI agents on a machine you own.
package main

import (
	"os"

	"github.com/Hunt4Bugs/agentsd/internal/cli"
)

func main() {
	os.Exit(cli.Main(os.Args[1:]))
}

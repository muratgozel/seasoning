// Command seasoning is an end-to-end encrypted relay that lets coding agents
// working on the same project see and message each other (docs/work/root/spec.md).
package main

import (
	"os"
	_ "time/tzdata" // local-time display on hosts without zoneinfo

	"github.com/muratgozel/seasoning/internal/cli"
)

func main() {
	os.Exit(cli.Main(os.Args[1:], os.Stdin, os.Stdout, os.Stderr))
}

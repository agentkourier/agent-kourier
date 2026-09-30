// Command courier is the agent-courier broker. See spec.md for the design.
package main

import (
	"flag"
	"fmt"
	"io"
	"os"
)

// version is set at build time: -ldflags "-X main.version=<value>".
var version = "dev"

func main() {
	os.Exit(run(os.Args[1:], version, os.Stdout, os.Stderr))
}

// run executes the CLI and returns the process exit code.
func run(args []string, ver string, stdout, stderr io.Writer) int {
	fs := flag.NewFlagSet("courier", flag.ContinueOnError)
	fs.SetOutput(stderr)
	showVersion := fs.Bool("version", false, "print the version and exit")
	if err := fs.Parse(args); err != nil {
		return 2
	}
	if *showVersion {
		_, _ = fmt.Fprintln(stdout, ver)
	}
	return 0
}

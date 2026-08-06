// Command pager delivers messages between coding-agent sessions, across
// projects and across tools.
//
// A sender leaves a message; the recipient sees it the next time their session
// is active. Delivery happens inside the recipient's hook, so there is no
// polling and no forced interruption.
package main

import (
	"errors"
	"fmt"
	"os"
	"text/tabwriter"
)

// errNotImplemented marks a subcommand that is dispatched but whose behaviour
// lands in a later implementation step.
var errNotImplemented = errors.New("not implemented yet")

// command is one pager subcommand. This table is the single source of truth
// for both dispatch and help output, so the two cannot drift apart.
type command struct {
	name    string
	args    string
	summary string
	run     func(args []string) error
}

var commands = []command{
	{"send", "<target> <body> [--human] [--session <id>]", "Leave a message for another session", todo},
	{"attach", "--tool <tool> --root <dir> [--session <id>]", "Register this session as a delivery target", todo},
	{"alias", "<name> [--session <id>]", "Point a short name at a session", todo},
	{"claim", "<name>", "Take over an alias whose session is offline", todo},
	{"ls", "[--expired]", "List messages addressed to this session", todo},
	{"whoami", "", "Show the session this invocation resolves to", todo},
	{"prune", "[--dry-run]", "Delete messages past the retention window", todo},
	{"hook", "<event>", "Hook adapter: deliver pending messages on stdout", todo},
	{"mcp", "", "Serve the MCP stdio server", todo},
}

func todo([]string) error { return errNotImplemented }

func main() {
	if err := run(os.Args[1:]); err != nil {
		fmt.Fprintf(os.Stderr, "pager: %v\n", err)
		os.Exit(1)
	}
}

func run(argv []string) error {
	if len(argv) == 0 {
		usage(os.Stdout)
		return nil
	}
	switch argv[0] {
	case "-h", "--help", "help":
		usage(os.Stdout)
		return nil
	}
	for _, c := range commands {
		if c.name == argv[0] {
			return c.run(argv[1:])
		}
	}
	usage(os.Stderr)
	return fmt.Errorf("unknown command %q", argv[0])
}

func usage(w *os.File) {
	fmt.Fprint(w, "pager — message delivery between coding-agent sessions.\n\n"+
		"Usage:\n  pager <command> [flags]\n\nCommands:\n")
	tw := tabwriter.NewWriter(w, 0, 0, 2, ' ', 0)
	for _, c := range commands {
		fmt.Fprintf(tw, "  %s %s\t%s\n", c.name, c.args, c.summary)
	}
	tw.Flush()
	fmt.Fprint(w, "\nSession binding: see docs/session-binding.md.\n"+
		"A send with no resolvable session is rejected unless --human is given.\n")
}

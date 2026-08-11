// Command pager delivers messages between coding-agent sessions, across
// projects and across tools.
//
// A sender leaves a message; the recipient sees it the next time their session
// is active. Delivery happens inside the recipient's hook, so there is no
// polling and no forced interruption.
package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"os"
	"text/tabwriter"

	"github.com/unghee/pager/internal/clock"
	"github.com/unghee/pager/internal/hookio"
	"github.com/unghee/pager/internal/sessionref"
	"github.com/unghee/pager/internal/store"
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
	{"attach", "[--session <id>] [--tool <tool>] [--root <dir>] [--pm-ref <ref>]", "Register this session as a delivery target", attach},
	{"alias", "<name> [--session <id>]", "Point a short name at a session", todo},
	{"claim", "<name>", "Take over an alias whose session is offline", todo},
	{"ls", "[--expired]", "List messages addressed to this session", todo},
	{"whoami", "[--session <id>]", "Show the session this invocation resolves to", whoami},
	{"prune", "[--dry-run]", "Delete messages past the retention window", todo},
	{"hook", "<event>", "Hook adapter: deliver pending messages on stdout", hookCmd},
	{"mcp", "", "Serve the MCP stdio server", todo},
}

func todo([]string) error { return errNotImplemented }

// openStore opens the standard database with the real clock.
func openStore(ctx context.Context) (*store.Store, error) {
	path, err := store.DefaultPath()
	if err != nil {
		return nil, err
	}
	return store.Open(ctx, path, clock.System{})
}

// resolver wires the shared session resolver to the database. Every sending
// path uses this one construction — see docs/session-binding.md.
func resolver(st *store.Store) *sessionref.Resolver {
	return sessionref.New(func(ctx context.Context, client string, host sessionref.Instance) (string, error) {
		return st.SessionByHost(ctx, client, host.Pid, host.Start, store.DefaultStale)
	})
}

// hookCmd runs one hook invocation and always succeeds.
//
// Nothing it can encounter is worth failing over: a non-zero exit or a stray
// line on stderr reaches the host, and a paging system that disrupts the
// sessions it pages is worse than one that quietly delivers nothing. The
// timeout exists for the same reason — a wedged database must not hold the
// session open.
func hookCmd(args []string) error {
	var event string
	if len(args) > 0 {
		event = args[0]
	}
	ctx, cancel := context.WithTimeout(context.Background(), hookio.Deadline)
	defer cancel()
	hookio.Run(ctx, event, os.Stdin, os.Stdout)
	return nil
}

// attach records the calling session and binds it to the host process, which
// is what every later host-based lookup reads. In normal operation the hook
// does this on every event; the command exists for setup and for verifying the
// binding by hand.
func attach(args []string) error {
	fs := flag.NewFlagSet("attach", flag.ContinueOnError)
	session := fs.String("session", "", "session id (required unless PAGER_SESSION is set)")
	tool := fs.String("tool", "", "host tool: claude or codex (default: the detected host)")
	root := fs.String("root", "", "workspace root (default: the working directory)")
	pmRef := fs.String("pm-ref", "", "optional KEY/id to address this session by")
	if err := fs.Parse(args); err != nil {
		return err
	}

	ctx := context.Background()
	st, err := openStore(ctx)
	if err != nil {
		return err
	}
	defer st.Close()

	ref, err := resolver(st).Resolve(ctx, *session)
	if err != nil {
		return err
	}
	// attach establishes the binding that the host tier reads, so it cannot
	// use that tier to learn its own identity.
	if ref.Source != sessionref.SourceFlag && ref.Source != sessionref.SourceEnv {
		return errors.New("attach needs an explicit session: pass --session or set PAGER_SESSION")
	}

	rec := store.SessionRecord{
		ID: ref.SessionID, Tool: *tool, Root: *root, PMRef: *pmRef,
		HostClient: ref.Client, HostPid: ref.Host.Pid, HostStart: ref.Host.Start,
	}
	if rec.Tool == "" {
		if ref.Client == sessionref.Unknown {
			return errors.New("no host detected, so --tool is required")
		}
		rec.Tool = ref.Client
	}
	if rec.Root == "" {
		if rec.Root, err = os.Getwd(); err != nil {
			return fmt.Errorf("resolve working directory: %w", err)
		}
	}
	if err := st.RecordSession(ctx, rec); err != nil {
		return err
	}

	fmt.Printf("attached %s (%s) at %s\n", rec.ID, rec.Tool, rec.Root)
	if !ref.Host.Valid() {
		fmt.Println("warning: no host process detected — this session is reachable only via --session or PAGER_SESSION")
	}
	return nil
}

// whoami reports what this invocation resolves to. It prints the detected host
// separately from the session because the two fail independently: a detected
// host with no session means nothing has recorded one yet, while no detected
// host means the ancestry walk itself came up empty.
func whoami(args []string) error {
	fs := flag.NewFlagSet("whoami", flag.ContinueOnError)
	session := fs.String("session", "", "session id to attribute this invocation to")
	if err := fs.Parse(args); err != nil {
		return err
	}

	ctx := context.Background()
	st, err := openStore(ctx)
	if err != nil {
		return err
	}
	defer st.Close()

	ref, err := resolver(st).Resolve(ctx, *session)
	if err != nil {
		return err
	}

	if ref.Host.Valid() {
		fmt.Printf("host:    %s pid=%d start=%d\n", ref.Client, ref.Host.Pid, ref.Host.Start)
	} else {
		fmt.Println("host:    not detected")
	}
	if ref.Attributed() {
		fmt.Printf("session: %s (via %s)\n", ref.SessionID, ref.Source)
	} else {
		fmt.Println("session: unattributed — sending needs --session, PAGER_SESSION, or --human")
	}
	return nil
}

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

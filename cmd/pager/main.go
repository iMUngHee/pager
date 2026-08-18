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
	"strings"
	"text/tabwriter"
	"time"

	"github.com/unghee/pager/internal/clock"
	"github.com/unghee/pager/internal/deliver"
	"github.com/unghee/pager/internal/hookio"
	"github.com/unghee/pager/internal/mcpsrv"
	"github.com/unghee/pager/internal/sessionref"
	"github.com/unghee/pager/internal/store"
)

// command is one pager subcommand. This table is the single source of truth
// for both dispatch and help output, so the two cannot drift apart.
type command struct {
	name    string
	args    string
	summary string
	run     func(args []string) error
}

var commands = []command{
	{"send", "<target> <body> [--human] [--session <id>] [--label <name>]", "Leave a message for another session", send},
	{"attach", "[--session <id>] [--tool <tool>] [--root <dir>] [--pm-ref <ref>]", "Register this session as a delivery target", attach},
	{"alias", "<name> [--session <id>]", "Point a short name at a session", alias},
	{"claim", "<name> [--session <id>]", "Take over an alias whose session is offline", claim},
	{"ls", "[--expired] [--session <id>]", "List messages addressed to this session", list},
	{"who", "", "List the sessions that can be paged right now", who},
	{"whoami", "[--session <id>]", "Show the session this invocation resolves to", whoami},
	{"export", "", "Write every stored message to stdout as JSONL", export},
	{"prune", "[--dry-run]", "Delete messages past the retention window", prune},
	{"hook", "<event>", "Hook adapter: deliver pending messages on stdout", hookCmd},
	{"mcp", "", "Serve the MCP stdio server", serveMCP},
}

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

// resolveSession applies the shared resolver and returns the session id, or an
// error naming what the caller can do about it.
func resolveSession(ctx context.Context, st *store.Store, flagValue string) (string, error) {
	ref, err := resolver(st).Resolve(ctx, flagValue)
	if err != nil {
		return "", err
	}
	if !ref.Attributed() {
		return "", deliver.ErrUnattributed
	}
	return ref.SessionID, nil
}

// send leaves a message for another session.
//
// The target is resolved before the send so an unknown or ambiguous name fails
// with the candidates rather than queueing mail nobody will read.
func send(args []string) error {
	fs := flag.NewFlagSet("send", flag.ContinueOnError)
	session := fs.String("session", "", "sending session id")
	human := fs.Bool("human", false, "assert this send is operator-initiated, not agent-initiated")
	label := fs.String("label", "", "how the recipient sees the sender")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if fs.NArg() < 2 {
		return errors.New("usage: pager send <target> <body>")
	}
	target, body := fs.Arg(0), strings.Join(fs.Args()[1:], " ")

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
	// --human is an operator assertion, so it is the one way to send without a
	// resolvable session. Everything else is refused rather than recorded
	// anonymously.
	if !ref.Attributed() && !*human {
		return deliver.ErrUnattributed
	}

	found, err := deliver.ResolveTarget(ctx, st, target)
	if err != nil {
		return err
	}
	if *label == "" {
		if *label, err = deliver.SenderLabel(ctx, st, ref.SessionID); err != nil {
			return err
		}
	}

	sent, err := deliver.Send(ctx, st, deliver.SendRequest{
		Alias: found.Alias, Body: body, Sender: ref.SessionID, Label: *label, Human: *human,
	})
	if err != nil {
		return err
	}
	fmt.Printf("sent #%d to %s (hop %d, %s)\n", sent.ID, found.Alias, sent.Hop, sent.Origin)
	if found.SessionID == "" {
		fmt.Printf("note: %s has no live session — it will be delivered when one claims the alias\n", found.Alias)
	}
	return nil
}

// alias points a name at this session.
func alias(args []string) error {
	fs := flag.NewFlagSet("alias", flag.ContinueOnError)
	session := fs.String("session", "", "session id to name")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if fs.NArg() != 1 {
		return errors.New("usage: pager alias <name>")
	}

	ctx := context.Background()
	st, err := openStore(ctx)
	if err != nil {
		return err
	}
	defer st.Close()

	id, err := resolveSession(ctx, st, *session)
	if err != nil {
		return err
	}
	ok, err := deliver.SetAlias(ctx, st, fs.Arg(0), id)
	if err != nil {
		return err
	}
	if !ok {
		return fmt.Errorf("%q belongs to another session; take it over with: pager claim %s", fs.Arg(0), fs.Arg(0))
	}
	fmt.Printf("%s now points at %s\n", fs.Arg(0), id)
	return nil
}

// claim takes over an alias whose holder is gone.
func claim(args []string) error {
	fs := flag.NewFlagSet("claim", flag.ContinueOnError)
	session := fs.String("session", "", "session id taking over")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if fs.NArg() != 1 {
		return errors.New("usage: pager claim <name>")
	}

	ctx := context.Background()
	st, err := openStore(ctx)
	if err != nil {
		return err
	}
	defer st.Close()

	id, err := resolveSession(ctx, st, *session)
	if err != nil {
		return err
	}
	ok, err := deliver.ClaimAlias(ctx, st, fs.Arg(0), id, store.DefaultStale)
	if err != nil {
		return err
	}
	if !ok {
		return fmt.Errorf("cannot take over %q: it does not exist, its holder is still active, or it belongs to another workspace", fs.Arg(0))
	}
	fmt.Printf("%s is now yours\n", fs.Arg(0))
	return nil
}

// list shows what is addressed to this session.
func list(args []string) error {
	fs := flag.NewFlagSet("ls", flag.ContinueOnError)
	session := fs.String("session", "", "session id to list for")
	expired := fs.Bool("expired", false, "show only messages past the automatic delivery window")
	if err := fs.Parse(args); err != nil {
		return err
	}

	ctx := context.Background()
	st, err := openStore(ctx)
	if err != nil {
		return err
	}
	defer st.Close()

	id, err := resolveSession(ctx, st, *session)
	if err != nil {
		return err
	}
	messages, err := deliver.List(ctx, st, id, *expired, deliver.LimitsFromEnv())
	if err != nil {
		return err
	}
	if len(messages) == 0 {
		fmt.Println("nothing here")
		return nil
	}

	tw := tabwriter.NewWriter(os.Stdout, 0, 0, 2, ' ', 0)
	fmt.Fprintln(tw, "ID\tINBOX\tFROM\tSTATE\tBODY")
	for _, m := range messages {
		fmt.Fprintf(tw, "#%d\t%s\t%s\t%s\t%s\n", m.ID, m.Alias, m.Sender, state(m), firstLine(m.Body))
	}
	return tw.Flush()
}

func state(m deliver.Listed) string {
	switch {
	case m.Delivered:
		return "delivered"
	case m.Expired:
		return "expired"
	default:
		return "waiting"
	}
}

func firstLine(body string) string {
	line, _, cut := strings.Cut(body, "\n")
	if cut {
		return line + " …"
	}
	return line
}

// export writes the whole database to stdout as JSONL.
//
// It takes no flags on purpose. Prune's archive uses the same record shape, so
// filtering is jq's job and saving is the shell's:
//
//	pager export > backup.jsonl
//	pager export | jq 'select(.alias == "gupa")'
//	cat ~/.pager/archive.jsonl <(pager export) | awk '!seen[$0]++'
//
// Nor is there a session to resolve: this dumps one local user's database
// whole, so there is nobody to attribute the call to.
//
// Unknown arguments are refused rather than ignored. Quietly dumping everything
// in response to `pager export --since 3d` would look like the flag worked.
func export(args []string) error {
	if len(args) > 0 {
		return fmt.Errorf("export takes no arguments (got %q) — filter the output with jq instead", args[0])
	}

	ctx := context.Background()
	st, err := openStore(ctx)
	if err != nil {
		return err
	}
	defer st.Close()

	_, err = st.ExportAll(ctx, os.Stdout)
	return err
}

// prune deletes messages past the retention window.
func prune(args []string) error {
	fs := flag.NewFlagSet("prune", flag.ContinueOnError)
	dryRun := fs.Bool("dry-run", false, "report what would be deleted, delete nothing")
	if err := fs.Parse(args); err != nil {
		return err
	}

	ctx := context.Background()
	st, err := openStore(ctx)
	if err != nil {
		return err
	}
	defer st.Close()

	res, err := st.PruneNow(ctx, store.DefaultRetention, *dryRun)
	if err != nil {
		return err
	}
	if *dryRun {
		fmt.Printf("%d message(s) would be deleted\n", res.Deleted)
		return nil
	}
	fmt.Printf("%d message(s) deleted\n", res.Deleted)
	return nil
}

// serveMCP runs the MCP stdio server until the client disconnects.
func serveMCP([]string) error {
	ctx := context.Background()
	st, err := openStore(ctx)
	if err != nil {
		return err
	}
	defer st.Close()
	return mcpsrv.New(st).Serve(ctx, os.Stdin, os.Stdout)
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

	// Name it here too. Hooks are the usual path, but setting a session up by
	// hand is a documented one, and a session that stays nameless is exactly
	// the situation this avoids — its messages would arrive signed with a UUID.
	//
	// Failing to name it does not fail the command, on the same judgement that
	// makes hookio drop this error: an unnamed session still works, it is only
	// harder to talk about. By this point RecordSession has already succeeded,
	// so returning here would report a completed attach as a failure — and it
	// would swallow the host warning below, which is the more actionable of the
	// two things that can be wrong.
	name, _, err := deliver.EnsureAutoAlias(ctx, st, rec.ID)
	switch {
	case err != nil:
		fmt.Printf("warning: this session has no name yet: %v\n", err)
	case name != "":
		fmt.Printf("name:     %s\n", name)
	}

	if !ref.Host.Valid() {
		fmt.Println("warning: no host process detected — this session is reachable only via --session or PAGER_SESSION")
	}
	return nil
}

// who lists the sessions that can be paged right now.
//
// This is how a person learns the names to address. A session is named without
// being asked, which is what makes paging possible at all, but a name nobody
// can see is a name nobody will use.
func who(args []string) error {
	fs := flag.NewFlagSet("who", flag.ContinueOnError)
	if err := fs.Parse(args); err != nil {
		return err
	}

	ctx := context.Background()
	st, err := openStore(ctx)
	if err != nil {
		return err
	}
	defer st.Close()

	entries, err := deliver.Roster(ctx, st, store.DefaultStale)
	if err != nil {
		return err
	}
	if len(entries) == 0 {
		fmt.Println("no sessions are active")
		return nil
	}

	now := st.Now()
	tw := tabwriter.NewWriter(os.Stdout, 0, 0, 2, ' ', 0)
	fmt.Fprintln(tw, "NAME\tTOOL\tROOT\tLAST")
	for _, e := range entries {
		fmt.Fprintf(tw, "%s\t%s\t%s\t%s\n", e.Name, e.Tool, e.Root, ago(now, e.LastSeen))
	}
	return tw.Flush()
}

// ago renders a heartbeat's age coarsely. The exact time is not the question a
// roster answers; "is this session still around" is.
func ago(now, then int64) string {
	d := time.Duration(now-then) * time.Millisecond
	switch {
	case d < time.Minute:
		return "just now"
	case d < time.Hour:
		return fmt.Sprintf("%dm ago", int(d.Minutes()))
	default:
		return fmt.Sprintf("%dh ago", int(d.Hours()))
	}
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
	if !ref.Attributed() {
		fmt.Println("session: unattributed — sending needs --session, PAGER_SESSION, or --human")
		return nil
	}
	fmt.Printf("session: %s (via %s)\n", ref.SessionID, ref.Source)

	// The name is printed separately from the session id because it is the
	// part a person uses, and because its absence is the symptom worth seeing:
	// a session with no name is one whose hooks have not run, or whose host
	// was never detected.
	name, err := deliver.PrimaryAlias(ctx, st, ref.SessionID)
	if err != nil {
		return err
	}
	if name == "" {
		fmt.Println("name:    none — it is assigned once a hook runs with the host detected")
		return nil
	}
	fmt.Printf("name:    %s\n", name)
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

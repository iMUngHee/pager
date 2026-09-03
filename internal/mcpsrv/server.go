// Package mcpsrv serves pager over MCP stdio, so an agent can page another
// session with a tool call instead of a shell command.
//
// MCP carries no session identity, and one server process serves one session,
// so the server resolves who it is exactly the way the CLI does: by detecting
// the host process it descends from and reading the session a hook recorded
// against it. See docs/session-binding.md.
package mcpsrv

import (
	"context"
	"fmt"
	"io"
	"strings"

	"github.com/mark3labs/mcp-go/mcp"
	mcpserver "github.com/mark3labs/mcp-go/server"

	"github.com/unghee/pager/internal/deliver"
	"github.com/unghee/pager/internal/sessionref"
	"github.com/unghee/pager/internal/store"
	"github.com/unghee/pager/internal/wake"
)

const (
	name    = "pager"
	version = "0.1.0"
)

// Server wraps an MCP stdio server over one pager database.
type Server struct {
	st  *store.Store
	mcp *mcpserver.MCPServer
	res *sessionref.Resolver
}

// New builds a server with every tool registered.
func New(st *store.Store) *Server {
	s := &Server{
		st: st,
		mcp: mcpserver.NewMCPServer(name, version,
			mcpserver.WithToolCapabilities(true)),
		res: sessionref.New(func(ctx context.Context, client string, host sessionref.Instance) (string, error) {
			return st.SessionByHost(ctx, client, host.Pid, host.Start, store.DefaultStale)
		}),
	}
	s.register()
	return s
}

// MCP exposes the underlying server so tests can drive it in process.
func (s *Server) MCP() *mcpserver.MCPServer { return s.mcp }

// Serve runs the stdio loop over the given streams until the client
// disconnects. Taking the streams as arguments is what lets a test drive a
// real JSON-RPC exchange in process.
func (s *Server) Serve(ctx context.Context, stdin io.Reader, stdout io.Writer) error {
	if err := mcpserver.NewStdioServer(s.mcp).Listen(ctx, stdin, stdout); err != nil {
		return fmt.Errorf("mcp stdio: %w", err)
	}
	return nil
}

func (s *Server) register() {
	s.mcp.AddTool(
		mcp.NewTool("msg_send",
			mcp.WithDescription(
				"Leave a message for another agent session. The recipient sees it the next time their session is active — "+
					"there is no interruption and no delivery receipt. Address them by alias, session id, or pm task ref."),
			mcp.WithString("target", mcp.Required(),
				mcp.Description("Who to page: an alias, a session id, a pm ref, or a unique fragment of an alias.")),
			mcp.WithString("body", mcp.Required(),
				mcp.Description("What to tell them. Long bodies are truncated on delivery.")),
			mcp.WithString("label",
				mcp.Description("How the recipient sees you. Defaults to your own alias.")),
		),
		s.handleSend,
	)

	s.mcp.AddTool(
		mcp.NewTool("msg_list",
			mcp.WithDescription(
				"List messages addressed to this session. Returns the whole history, delivered ones included, unless narrowed."),
			mcp.WithBoolean("waiting",
				mcp.Description("Show only messages nobody has dealt with yet — neither injected by a hook nor returned by an earlier listing. The cheap way to poll during a long turn, and usually empty.")),
			mcp.WithBoolean("expired",
				mcp.Description("Show only messages past the automatic delivery window — these are not retried and need resending by hand.")),
		),
		s.handleList,
	)

	s.mcp.AddTool(
		mcp.NewTool("msg_roster",
			mcp.WithDescription(
				"List the sessions you can page right now — the companion to msg_send, which needs a name to address. "+
					"HOST says whether the recorded process is still running: live, gone, or unknown when there is nothing to ask about. "+
					"LAST is only when a hook last ran for that session, so a session working through a long turn can look old and one "+
					"that died minutes ago can look recent. Address anyone here by the name in NAME."),
		),
		s.handleRoster,
	)
}

// session resolves who this call is from. There is no way for a tool argument
// to override it: letting a caller name its own sender would make the causal
// depth it inherits a matter of choice.
func (s *Server) session(ctx context.Context) (string, error) {
	ref, err := s.res.Resolve(ctx, "")
	if err != nil {
		return "", err
	}
	if !ref.Attributed() {
		return "", fmt.Errorf("%w (no hook has recorded this session yet)", deliver.ErrUnattributed)
	}
	return ref.SessionID, nil
}

func (s *Server) handleSend(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
	args := req.GetArguments()
	target, _ := args["target"].(string)
	body, _ := args["body"].(string)
	label, _ := args["label"].(string)
	if strings.TrimSpace(target) == "" || strings.TrimSpace(body) == "" {
		return mcp.NewToolResultError("target and body are both required"), nil
	}

	sender, err := s.session(ctx)
	if err != nil {
		return mcp.NewToolResultError(err.Error()), nil
	}
	found, err := deliver.ResolveTarget(ctx, s.st, target, store.DefaultStale)
	if err != nil {
		return mcp.NewToolResultError(err.Error()), nil
	}
	if label == "" {
		if label, err = deliver.SenderLabel(ctx, s.st, sender); err != nil {
			return mcp.NewToolResultError(err.Error()), nil
		}
	}

	// Human is never asserted here. It is an operator's claim about themselves,
	// and everything arriving over MCP is by definition an agent acting.
	sent, err := deliver.Send(ctx, s.st, deliver.SendRequest{
		Alias: found.Alias, Body: body, Sender: sender, Label: label,
	})
	if err != nil {
		return mcp.NewToolResultError(err.Error()), nil
	}

	out := fmt.Sprintf("sent #%d to %s (hop %d, %s)", sent.ID, found.Alias, sent.Hop, sent.Origin)

	// Same question, same sentence as the CLI. It used to be answered here in
	// wording of its own, and the two drifted precisely because the branch was
	// unreachable and so nobody read either copy.
	presence, err := deliver.PresenceOf(ctx, s.st, found, sessionref.AliveAt)
	if err != nil {
		return mcp.NewToolResultError(err.Error()), nil
	}
	if line := presence.Note(found.Alias); line != "" {
		out += "\n" + line
	} else if line := wake.Describe(
		wake.Wake(ctx, found.SessionID, wake.PokeBody(found.Alias, label)), found.Alias,
	); line != "" {
		// Reported, never fatal. The message is stored either way; a poke only
		// moves when the recipient reads it.
		out += "\n" + line
	}
	return mcp.NewToolResultText(out), nil
}

// handleRoster answers who is reachable.
//
// It resolves no session of its own, which is the one place this server departs
// from the other two tools. They act as the caller — msg_send stores it as the
// sender, msg_list reads and marks its inbox — so neither can proceed without
// knowing who it is. A roster is a question about everyone else, and refusing it
// to an unattributed caller would leave a session that no hook has recorded yet
// with no way to learn a name at all, which is the situation this tool exists to
// end.
//
// The rendering is deliver.FormatRoster, the same call `pager who` makes.
func (s *Server) handleRoster(ctx context.Context, _ mcp.CallToolRequest) (*mcp.CallToolResult, error) {
	entries, err := deliver.Roster(ctx, s.st, store.DefaultStale)
	if err != nil {
		return mcp.NewToolResultError(err.Error()), nil
	}
	return mcp.NewToolResultText(deliver.FormatRoster(entries, s.st.Now(), sessionref.AliveAt)), nil
}

func (s *Server) handleList(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
	args := req.GetArguments()
	waiting, _ := args["waiting"].(bool)
	expired, _ := args["expired"].(bool)

	session, err := s.session(ctx)
	if err != nil {
		return mcp.NewToolResultError(err.Error()), nil
	}
	messages, err := deliver.List(ctx, s.st, session, deliver.FilterFrom(waiting, expired), deliver.LimitsFromEnv())
	if err != nil {
		return mcp.NewToolResultError(err.Error()), nil
	}
	if len(messages) == 0 {
		return mcp.NewToolResultText("nothing here"), nil
	}

	// This is the only place a read is recorded. deliver.List is shared with
	// `pager ls`, which can be pointed at another session's inbox, so stamping
	// there would let a person glancing at a queue consume an agent's mail.
	// msg_list has no target argument at all, so this call can only ever mark
	// what belongs to the caller.
	var fresh []int64
	for _, m := range messages {
		if !m.Seen && !m.Delivered {
			fresh = append(fresh, m.ID)
		}
	}
	_, markErr := deliver.MarkListed(ctx, s.st, fresh)

	// The rendering below reads the slice as it was scanned, before the stamp,
	// so the listing that first surfaces a message still prints it as waiting
	// and only a later one calls it seen. Recomputing the state after the write
	// would be the other defensible choice, but it would mean a message is
	// reported as already seen by the very response that is showing it.
	var sb strings.Builder
	for _, m := range messages {
		fmt.Fprintf(&sb, "#%d [%s] %s -> %s: %s\n", m.ID, m.State(), m.Sender, m.Alias, m.Body)
	}
	// A failed stamp must not cost the caller its mail: it already has the
	// messages in hand, and losing them to bookkeeping would be the worse
	// outcome. Saying so is not optional either -- silence here would look
	// exactly like a working poll while every later one repeated itself.
	if markErr != nil {
		fmt.Fprintf(&sb, "\nnote: could not record these as read (%v); they will be listed again\n", markErr)
	}
	return mcp.NewToolResultText(sb.String()), nil
}

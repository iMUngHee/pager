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

// New builds a server with both tools registered.
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
				mcp.Description("Show only messages not delivered yet — the cheap way to poll during a long turn, and usually empty.")),
			mcp.WithBoolean("expired",
				mcp.Description("Show only messages past the automatic delivery window — these are not retried and need resending by hand.")),
		),
		s.handleList,
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
	if found.SessionID == "" {
		out += "\nnote: no live session holds that alias yet; it will be delivered when one claims it"
	}
	return mcp.NewToolResultText(out), nil
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

	var sb strings.Builder
	for _, m := range messages {
		fmt.Fprintf(&sb, "#%d [%s] %s -> %s: %s\n", m.ID, m.State(), m.Sender, m.Alias, m.Body)
	}
	return mcp.NewToolResultText(sb.String()), nil
}

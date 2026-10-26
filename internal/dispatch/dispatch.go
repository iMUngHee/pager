// Package dispatch is the one send path the CLI and the MCP server share:
// classify the target, write the message, then say what is behind the name.
//
// It exists because the two used to carry their own copies of that sequence,
// and the copies drifted — the presence note was once worded differently on
// each side precisely because nobody read both. It cannot live in deliver:
// the last step pokes the recipient, and wake already imports deliver.
package dispatch

import (
	"context"
	"fmt"

	"github.com/iMUngHee/pager/internal/deliver"
	"github.com/iMUngHee/pager/internal/sessionref"
	"github.com/iMUngHee/pager/internal/store"
	"github.com/iMUngHee/pager/internal/wake"
)

// Request is one send as the sender expressed it.
type Request struct {
	Target string // alias, session id, pm ref, fragment, or name@peer
	Body   string
	Sender string // resolved sending session; "" when unattributed
	Label  string // how the recipient sees the sender; "" for the default
	Human  bool   // operator assertion; never set by the MCP server

	// Probe answers whether a recorded host is still running. nil means
	// sessionref.AliveAt; tests hand in one that answers without a process.
	Probe deliver.HostProbe
}

// Result is what was written and what the sender should be told about it.
type Result struct {
	Sent deliver.Sent
	// Address is the alias, or name@peer, the message was written to.
	Address string
	// Lines are the notes that follow Head, in order. Empty when there is
	// nothing to add.
	Lines []string
}

// Head is the first line both front ends print for a send.
func (r Result) Head() string {
	return fmt.Sprintf("sent #%d to %s (hop %d, %s)", r.Sent.ID, r.Address, r.Sent.Hop, r.Sent.Origin)
}

// Send resolves req.Target, writes the message and reports on it.
//
// Once the message is stored, a failure in the steps that only report on it —
// who is behind the name, the poke — is returned together with the Result, so
// a caller can still say what was sent: res.Sent.ID is non-zero exactly when
// the message exists. Dropping that would invite a retry that sends it twice.
//
// The target is resolved before anything is written, so an unknown or
// ambiguous name fails with the candidates rather than queueing mail nobody
// will read. A remote address is never poked: the recipient is in another
// store, and nothing here can reach it — the relay carries the message, and
// the receiving store wakes its own session when it imports it.
func Send(ctx context.Context, st *store.Store, req Request) (Result, error) {
	addr, err := deliver.ParseAddress(ctx, st, req.Target)
	if err != nil {
		return Result{}, err
	}

	if addr.Remote() {
		label, err := senderLabel(ctx, st, req)
		if err != nil {
			return Result{}, err
		}
		address := addr.Ref + "@" + addr.Peer
		sent, err := deliver.Send(ctx, st, deliver.SendRequest{
			Alias: address, Peer: addr.Peer, Body: req.Body, Sender: req.Sender, Label: label, Human: req.Human,
		})
		if err != nil {
			return Result{}, err
		}
		return Result{Sent: sent, Address: address, Lines: []string{
			"queued for peer " + addr.Peer + "; it leaves when the relay next runs",
		}}, nil
	}

	var found deliver.Target
	if addr.Exact {
		found, err = deliver.ResolveExact(ctx, st, addr.Ref)
	} else {
		found, err = deliver.ResolveTarget(ctx, st, addr.Ref, store.DefaultStale)
	}
	if err != nil {
		return Result{}, err
	}
	label, err := senderLabel(ctx, st, req)
	if err != nil {
		return Result{}, err
	}

	sent, err := deliver.Send(ctx, st, deliver.SendRequest{
		Alias: found.Alias, Body: req.Body, Sender: req.Sender, Label: label, Human: req.Human,
	})
	if err != nil {
		return Result{}, err
	}
	res := Result{Sent: sent, Address: found.Alias}

	// Who is behind the name decides whether poking it means anything. A
	// stranded holder cannot answer a poke, and telling the sender it "will be
	// delivered on its next activity" — which is what wake says of anything it
	// could not reach — promises an activity that is not coming.
	probe := req.Probe
	if probe == nil {
		probe = sessionref.AliveAt
	}
	presence, err := deliver.PresenceOf(ctx, st, found, probe)
	if err != nil {
		return res, err
	}
	if line := presence.Note(found.Alias); line != "" {
		res.Lines = append(res.Lines, line)
		return res, nil
	}

	// The message is already stored, so nothing below can cost a delivery. A
	// poke only decides whether the recipient reads it now or the next time it
	// runs, which is why wake reports an outcome instead of returning an error.
	tool, err := deliver.SenderTool(ctx, st, req.Sender)
	if err != nil {
		return res, err
	}
	if line := wake.Describe(wake.Wake(ctx, found.SessionID, wake.PokeBody(found.Alias, label, tool)), found.Alias); line != "" {
		res.Lines = append(res.Lines, line)
	}
	return res, nil
}

// senderLabel is req.Label, or the sender's default label when none was given.
func senderLabel(ctx context.Context, st *store.Store, req Request) (string, error) {
	if req.Label != "" {
		return req.Label, nil
	}
	return deliver.SenderLabel(ctx, st, req.Sender)
}

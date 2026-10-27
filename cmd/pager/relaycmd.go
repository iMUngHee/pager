package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"os"
	"os/signal"
	"slices"
	"syscall"
	"text/tabwriter"

	"github.com/iMUngHee/pager/internal/deliver"
	"github.com/iMUngHee/pager/internal/relay"
)

// peerCmd manages the stores this one relays with.
func peerCmd(args []string) error {
	if len(args) == 0 {
		return usageError("usage: pager peer add <name> --store <id> | ls | rm <name> [--force] | rebind <name> --store <id>")
	}
	ctx := context.Background()
	switch args[0] {
	case "add":
		fs := flag.NewFlagSet("peer add", flag.ContinueOnError)
		storeID := fs.String("store", "", "the other store's id, from `pager relay id` there")
		if err := fs.Parse(permute(fs, args[1:])); err != nil {
			return usageError(err.Error())
		}
		if fs.NArg() != 1 || *storeID == "" {
			return usageError("usage: pager peer add <name> --store <id>")
		}
		st, err := openStore(ctx)
		if err != nil {
			return err
		}
		defer st.Close()
		p, err := deliver.AddPeer(ctx, st, fs.Arg(0), *storeID)
		if err != nil {
			return err
		}
		fmt.Printf("added peer %s (store %s) — address its sessions as <name>@%s\n", p.Name, p.StoreID, p.Name)
		return nil

	case "ls":
		if len(args) != 1 {
			return usageError("usage: pager peer ls")
		}
		st, err := openStore(ctx)
		if err != nil {
			return err
		}
		defer st.Close()
		peers, err := deliver.ListPeers(ctx, st)
		if err != nil {
			return err
		}
		if len(peers) == 0 {
			fmt.Println("no peers — register one with: pager peer add <name> --store <id>")
			return nil
		}
		// SEQ, not SENT: out_seq belongs to the current link and starts over
		// when a rebind or new-id mints another, and a lifetime count could not
		// be had anyway once prune has taken the rows that made it up.
		tw := tabwriter.NewWriter(os.Stdout, 0, 0, 2, ' ', 0)
		fmt.Fprintln(tw, "NAME\tSTORE\tPENDING\tSEQ")
		for _, p := range peers {
			fmt.Fprintf(tw, "%s\t%s\t%d\t%d\n", p.Name, p.StoreID, p.Pending, p.OutSeq)
		}
		return tw.Flush()

	case "rebind":
		fs := flag.NewFlagSet("peer rebind", flag.ContinueOnError)
		storeID := fs.String("store", "", "the store id to pair with now, from `pager relay id` there")
		if err := fs.Parse(permute(fs, args[1:])); err != nil {
			return usageError(err.Error())
		}
		if fs.NArg() != 1 || *storeID == "" {
			return usageError("usage: pager peer rebind <name> --store <id>")
		}
		st, err := openStore(ctx)
		if err != nil {
			return err
		}
		defer st.Close()
		p, err := deliver.RebindPeer(ctx, st, fs.Arg(0), *storeID)
		if err != nil {
			return err
		}
		fmt.Printf("rebound peer %s to store %s on a new link; %d pending message(s) renumbered\n", p.Name, p.StoreID, p.Pending)
		return nil

	case "rm":
		fs := flag.NewFlagSet("peer rm", flag.ContinueOnError)
		force := fs.Bool("force", false, "cancel mail still waiting for this peer, telling each sender")
		if err := fs.Parse(permute(fs, args[1:])); err != nil {
			return usageError(err.Error())
		}
		if fs.NArg() != 1 {
			return usageError("usage: pager peer rm <name> [--force]")
		}
		st, err := openStore(ctx)
		if err != nil {
			return err
		}
		defer st.Close()
		if *force {
			return relay.ForceRemovePeer(ctx, st, fs.Arg(0), os.Stderr)
		}
		if err := deliver.RemovePeer(ctx, st, fs.Arg(0)); err != nil {
			return err
		}
		fmt.Printf("removed peer %s\n", fs.Arg(0))
		return nil
	}
	return usageError(fmt.Sprintf("unknown peer subcommand %q", args[0]))
}

// relayCmd is the store-side half of a relay. A transport runs these; pager
// itself never opens a connection.
func relayCmd(args []string) error {
	if len(args) == 0 {
		return usageError("usage: pager relay id | new-id [--force] | out --peer <p> [--limit n] | in --peer <p> | ack --peer <p>")
	}

	// A transport that stops reading closes the pipe under us. Ignoring
	// SIGPIPE turns that into a write error, which the commands treat as
	// "stop" — so relay in still pokes the sessions it already delivered to
	// instead of dying mid-run. An interrupt likewise stops between records,
	// after the current one has committed or rolled back.
	signal.Ignore(syscall.SIGPIPE)
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	sub := args[0]
	fs := flag.NewFlagSet("relay "+sub, flag.ContinueOnError)
	peer := fs.String("peer", "", "the peer this stream goes to or comes from")
	limit := fs.Int("limit", relay.DefaultLimit, fmt.Sprintf("most records to emit (1-%d)", relay.MaxLimit))
	force := fs.Bool("force", false, "new-id: cancel mail still waiting for peers, telling each sender")
	if err := fs.Parse(permute(fs, args[1:])); err != nil {
		return usageError(err.Error())
	}
	if fs.NArg() != 0 {
		return usageError(fmt.Sprintf("relay %s takes no arguments (got %q)", sub, fs.Arg(0)))
	}
	// Each subcommand takes only its own flags; one given to the wrong
	// subcommand is refused rather than quietly ignored.
	takes := map[string][]string{
		"id": nil, "new-id": {"force"}, "out": {"peer", "limit"}, "in": {"peer"}, "ack": {"peer"},
	}
	allowed, known := takes[sub]
	if !known {
		return usageError(fmt.Sprintf("unknown relay subcommand %q", sub))
	}
	var stray error
	fs.Visit(func(f *flag.Flag) {
		if stray == nil && !slices.Contains(allowed, f.Name) {
			stray = usageError(fmt.Sprintf("relay %s does not take --%s", sub, f.Name))
		}
	})
	if stray != nil {
		return stray
	}
	if slices.Contains(allowed, "peer") && *peer == "" {
		return usageError(fmt.Sprintf("relay %s needs --peer", sub))
	}

	st, err := openStore(ctx)
	if err != nil {
		return err
	}
	defer st.Close()

	switch sub {
	case "id":
		id, err := st.ID(ctx)
		if err != nil {
			return err
		}
		fmt.Println(id)
		return nil
	case "new-id":
		var id string
		if *force {
			id, err = relay.ForceRotateStoreID(ctx, st, os.Stderr)
		} else {
			id, err = deliver.RotateStoreID(ctx, st)
		}
		if err != nil {
			return err
		}
		fmt.Println(id)
		fmt.Fprintln(os.Stderr, "every peer must now run: pager peer rebind <name-of-this-store> --store "+id)
		return nil
	case "out":
		return relay.Out(ctx, st, *peer, *limit, os.Stdout)
	case "in":
		return relay.In(ctx, st, *peer, os.Stdin, os.Stdout, os.Stderr)
	case "ack":
		return relay.Ack(ctx, st, *peer, os.Stdin, os.Stderr)
	}
	return usageError(fmt.Sprintf("unknown relay subcommand %q", sub))
}

// usageError is a usage mistake, which a transport must not mistake for a
// cut stream it can retry.
func usageError(msg string) error {
	return &relay.ExitError{Code: 2, Err: errors.New(msg)}
}

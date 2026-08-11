package deliver

import (
	"context"
	"sync"
	"testing"
)

// These tests use a barrier rather than hoping goroutines overlap. Without one
// they would usually run in sequence and pass regardless of whether the
// reservation is atomic.

// TestSeqAllocatorConcurrent: several runs claiming at once must receive
// disjoint sequence ranges. A collision would let one confirmation block
// another arbitrarily, since the causal update only accepts an increase.
func TestSeqAllocatorConcurrent(t *testing.T) {
	st, _ := newStore(t)
	lim := DefaultLimits()
	inboxSession(t, st, "B", "inboxB")

	const runners, each = 4, 3
	ids := make([][]int64, runners)
	for r := range runners {
		for range each {
			ids[r] = append(ids[r], queue(t, st, "inboxB", "@a", "x"))
		}
	}

	start := make(chan struct{})
	var wg sync.WaitGroup
	claimed := make([][]int64, runners)
	errs := make([]error, runners)
	for r := range runners {
		wg.Go(func() {
			<-start
			_, claimed[r], errs[r] = Claim(context.Background(), st, "B", ids[r], lim)
		})
	}
	close(start)
	wg.Wait()

	seen := map[int64]int64{} // sequence -> message id
	total := 0
	for r, err := range errs {
		if err != nil {
			t.Fatalf("runner %d: %v", r, err)
		}
		for _, id := range claimed[r] {
			seq := seqOf(t, st, id)
			if seq == 0 {
				t.Errorf("message %d was claimed without a sequence number", id)
				continue
			}
			if prior, dup := seen[seq]; dup {
				t.Errorf("sequence %d issued to both message %d and %d", seq, prior, id)
			}
			seen[seq] = id
			total++
		}
	}
	if total != runners*each {
		t.Errorf("%d messages claimed in total, want %d", total, runners*each)
	}
	if got := cursorOf(t, st, "B"); got != int64(runners*each) {
		t.Errorf("seq_cursor = %d, want %d", got, runners*each)
	}
	// The reserved ranges must tile 1..N with no gap, since every claim
	// succeeded.
	for want := int64(1); want <= int64(total); want++ {
		if _, ok := seen[want]; !ok {
			t.Errorf("sequence %d was reserved but never assigned", want)
		}
	}
}

// TestConcurrentClaim: one message, many simultaneous runs, exactly one winner.
// Two winners would mean the same instruction delivered twice in one turn.
func TestConcurrentClaim(t *testing.T) {
	st, _ := newStore(t)
	lim := DefaultLimits()
	inboxSession(t, st, "B", "inboxB")
	id := queue(t, st, "inboxB", "@a", "only once")

	const runners = 8
	start := make(chan struct{})
	var wg sync.WaitGroup
	claimed := make([][]int64, runners)
	errs := make([]error, runners)
	for r := range runners {
		wg.Go(func() {
			<-start
			_, claimed[r], errs[r] = Claim(context.Background(), st, "B", []int64{id}, lim)
		})
	}
	close(start)
	wg.Wait()

	winners := 0
	for r, err := range errs {
		if err != nil {
			t.Fatalf("runner %d: %v", r, err)
		}
		if len(claimed[r]) > 0 {
			winners++
		}
	}
	if winners != 1 {
		t.Errorf("%d runs claimed the same message, want 1", winners)
	}
}

// TestConcurrentCollectBatch drives the whole path — candidates, budget, claim
// — from several runs at once, the way concurrent hooks in one session do.
func TestConcurrentCollectBatch(t *testing.T) {
	st, _ := newStore(t)
	lim := DefaultLimits()
	inboxSession(t, st, "B", "inboxB")

	const messages = 12
	for range messages {
		queue(t, st, "inboxB", "@a", "x")
	}

	const runners = 6
	start := make(chan struct{})
	var wg sync.WaitGroup
	batches := make([]Batch, runners)
	errs := make([]error, runners)
	for r := range runners {
		wg.Go(func() {
			<-start
			batches[r], errs[r] = CollectBatch(context.Background(), st, "B", lim)
		})
	}
	close(start)
	wg.Wait()

	seen := map[int64]bool{}
	for r, err := range errs {
		if err != nil {
			t.Fatalf("runner %d: %v", r, err)
		}
		for _, m := range batches[r].Messages {
			if seen[m.ID] {
				t.Errorf("message %d was handed to two runs at once", m.ID)
			}
			seen[m.ID] = true
		}
	}
	if len(seen) == 0 {
		t.Error("no run delivered anything")
	}
}

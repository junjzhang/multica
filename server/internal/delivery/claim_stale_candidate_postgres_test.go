package delivery

import (
	"context"
	"strings"
	"testing"

	"github.com/jackc/pgx/v5"
	db "github.com/multica-ai/multica/server/pkg/db/generated"
)

// pausingClaimTx parks the real Claim after its candidate list and before the
// per-session advisory lock, so another worker can lease and settle the row
// while this claim still holds the old "pending" hint.
type pausingClaimTx struct {
	pgx.Tx
	reached chan struct{}
	release chan struct{}
	once    bool
}

func (p *pausingClaimTx) QueryRow(ctx context.Context, sql string, args ...any) pgx.Row {
	if !p.once && strings.Contains(sql, "pg_try_advisory_xact_lock") {
		p.once = true
		close(p.reached)
		<-p.release
	}
	return p.Tx.QueryRow(ctx, sql, args...)
}

type pausingClaimBeginner struct {
	inner TxBeginner
	tx    *pausingClaimTx
}

func (b *pausingClaimBeginner) Begin(ctx context.Context) (pgx.Tx, error) {
	tx, err := b.inner.Begin(ctx)
	if err != nil {
		return nil, err
	}
	b.tx.Tx = tx
	return b.tx, nil
}

// A candidate row is only a pre-lock hint. If another worker leases it and the
// host may have accepted the request, the reclaimed lease must be
// verification-only. The test drives the returned lease through the real
// worker and counts host sends/lookups, so a wrong boolean cannot pass by
// itself.
func runStaleCandidateClaim(t *testing.T, recordUnknown bool) {
	t.Helper()
	pool := openDeliveryTestPool(t)
	workspaceID, agentID, ownerID, daemonID := newClaimFixture(t, pool)
	created := enqueueGeneric(t, pool, workspaceID, agentID, "run-stale-candidate")
	ctx := context.Background()

	ptx := &pausingClaimTx{reached: make(chan struct{}), release: make(chan struct{})}
	claimA := NewQueue(db.New(pool), &pausingClaimBeginner{inner: pool, tx: ptx})
	type claimResult struct {
		claims []Claim
		err    error
	}
	done := make(chan claimResult, 1)
	go func() {
		claims, err := claimA.Claim(ctx, ClaimRequest{
			Scope:       Scope{WorkspaceID: workspaceID},
			DaemonID:    daemonID,
			OwnerUserID: ownerID,
		})
		done <- claimResult{claims: claims, err: err}
	}()
	<-ptx.reached // A holds a "pending" candidate snapshot and no row lock.

	// B leases the row and sends. The host accepts; B loses its acknowledgement
	// (or records unknown and later reaches its retry time).
	queueB := NewQueue(db.New(pool), pool)
	b := claimForTest(t, queueB, workspaceID, daemonID, ownerID)
	if len(b) != 1 || b[0].ID != created.ID || b[0].VerifyOnly {
		close(ptx.release)
		t.Fatalf("B claim = %+v", b)
	}
	hostB := newFakeHost()
	if _, err := hostB.DeliverIfIdle(ctx, b[0].Session, b[0].Delivery); err != nil {
		close(ptx.release)
		t.Fatalf("B send: %v", err)
	}
	if hostB.deliverCount() != 1 {
		close(ptx.release)
		t.Fatalf("B sends = %d, want 1", hostB.deliverCount())
	}
	if recordUnknown {
		if _, err := queueB.RecordOutcome(ctx, OutcomeRequest{
			Scope:      Scope{WorkspaceID: workspaceID},
			DeliveryID: created.ID,
			DaemonID:   daemonID,
			LeaseToken: b[0].LeaseToken,
			State:      OutcomeUnknown,
			Error:      "ack timeout after send",
		}); err != nil {
			close(ptx.release)
			t.Fatalf("record unknown: %v", err)
		}
		makeClaimableNow(t, pool, created.ID)
	} else {
		expireLease(t, pool, created.ID)
	}

	close(ptx.release)
	result := <-done
	if result.err != nil {
		t.Fatalf("A claim: %v", result.err)
	}
	if len(result.claims) != 1 {
		t.Fatalf("A claims = %+v", result.claims)
	}

	// Run A's lease through the worker. A verification-only lease must look up
	// the original receipt and never call the host send path.
	client := newFakeOutcomeClient()
	client.claims["ws-1"] = [][]Claim{{result.claims[0]}}
	hostA := newFakeHost()
	worker := NewWorker(client, hostA, func() []string { return []string{"ws-1"} }, daemonID, nil)
	worker.round(ctx)

	if sends := hostA.deliverCount(); sends != 0 {
		t.Fatalf("worker sent a possibly-accepted row again: sends=%d (VerifyOnly=%v, attempts=%d)",
			sends, result.claims[0].VerifyOnly, result.claims[0].Attempts)
	}
	if lookups := hostA.receiptCalls; lookups != 1 {
		t.Fatalf("worker receipt lookups = %d, want exactly 1", lookups)
	}
	if !result.claims[0].VerifyOnly {
		t.Fatalf("A re-leased a possibly-accepted row (attempts=%d) as a send", result.claims[0].Attempts)
	}
	acks := client.ackSnapshot()
	if len(acks) != 1 || acks[0].request.State != OutcomeUnknown {
		t.Fatalf("worker outcome = %+v, want one unknown result", acks)
	}
}

func TestStaleCandidateExpiredLeaseIsVerifyOnly(t *testing.T) {
	runStaleCandidateClaim(t, false)
}

func TestStaleCandidateUnknownIsVerifyOnly(t *testing.T) {
	runStaleCandidateClaim(t, true)
}

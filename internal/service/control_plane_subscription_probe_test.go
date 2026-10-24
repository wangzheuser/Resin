package service

import (
	"context"
	"errors"
	"fmt"
	"testing"
	"time"

	"github.com/Resinat/Resin/internal/node"
	"github.com/Resinat/Resin/internal/probe"
	"github.com/Resinat/Resin/internal/subscription"
	"github.com/Resinat/Resin/internal/testutil"
)

func awaitSubscriptionProbe(t *testing.T, cp *ControlPlaneService) *SubscriptionProbeStatus {
	t.Helper()
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		status := cp.SubscriptionProbeStatus()
		if status != nil && (status.Status == "completed" || status.Status == "cancelled") {
			return status
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatal("subscription probe did not finish")
	return nil
}

func TestSubscriptionProbeCompletionAndConflicts(t *testing.T) {
	cp, mgr, pool := newCleanupSubscriptionTestService()
	sub := subscription.NewSubscription("batch", "batch", "https://example.com", true, false)
	mgr.Register(sub)
	mgr.Register(subscription.NewSubscription("other", "other", "https://example.com", true, false))
	var failed node.Hash
	for i := 0; i < 4; i++ {
		raw := []byte(fmt.Sprintf(`{"type":"batch","id":%d}`, i))
		hash := node.HashFromRawOptions(raw)
		sub.ManagedNodes().StoreNode(hash, subscription.ManagedNode{})
		if i == 3 {
			continue
		} // Missing during a refresh is skipped.
		pool.AddNodeFromSub(hash, raw, sub.ID)
		entry, _ := pool.GetEntry(hash)
		if i < 2 {
			ob := testutil.NewNoopOutbound()
			entry.Outbound.Store(&ob)
		} else {
			entry.SetLastError("outbound build failed")
		}
		if i == 1 {
			failed = hash
		}
	}
	gate := make(chan struct{})
	cp.ProbeMgr = probe.NewProbeManager(probe.ProbeConfig{Pool: pool, Concurrency: 1,
		ContextFetcher: func(ctx context.Context, hash node.Hash, _ string) ([]byte, time.Duration, error) {
			select {
			case <-ctx.Done():
				return nil, 0, ctx.Err()
			case <-gate:
			}
			if hash == failed {
				return nil, 0, errors.New("connection refused")
			}
			return []byte("ip=203.0.113.1\nloc=US\n"), time.Millisecond, nil
		}})
	cp.ProbeMgr.Start()
	t.Cleanup(cp.ProbeMgr.Stop)
	job, err := cp.StartSubscriptionProbe(sub.ID)
	if err != nil {
		t.Fatal(err)
	}
	duplicate, err := cp.StartSubscriptionProbe(sub.ID)
	if err != nil || duplicate.ID != job.ID {
		t.Fatal("duplicate start must reuse active job")
	}
	if _, err := cp.StartSubscriptionProbe("other"); err == nil {
		t.Fatal("another job should conflict")
	}
	if _, err := cp.CancelSubscriptionProbe("old-job"); err == nil {
		t.Fatal("stale job ID must not cancel current job")
	}
	close(gate)
	status := awaitSubscriptionProbe(t, cp)
	if status.Status != "completed" || status.Total != 4 || status.Completed != 4 ||
		status.Succeeded != 1 || status.Failed != 1 || status.Skipped != 2 {
		t.Fatalf("wrong totals: %+v", status)
	}
	sub.SetEnabled(false)
	if _, err := cp.StartSubscriptionProbe(sub.ID); err == nil {
		t.Fatal("disabled subscription started")
	}
}

func TestSubscriptionProbeCancelDoesNotPenalizeNode(t *testing.T) {
	cp, mgr, pool := newCleanupSubscriptionTestService()
	sub := subscription.NewSubscription("cancel", "cancel", "https://example.com", true, false)
	mgr.Register(sub)
	raw := []byte(`{"type":"cancel"}`)
	hash := node.HashFromRawOptions(raw)
	sub.ManagedNodes().StoreNode(hash, subscription.ManagedNode{})
	pool.AddNodeFromSub(hash, raw, sub.ID)
	entry, _ := pool.GetEntry(hash)
	ob := testutil.NewNoopOutbound()
	entry.Outbound.Store(&ob)
	entry.CircuitOpenSince.Store(0)
	started := make(chan struct{}, 1)
	cp.ProbeMgr = probe.NewProbeManager(probe.ProbeConfig{Pool: pool, Concurrency: 1,
		ContextFetcher: func(ctx context.Context, _ node.Hash, _ string) ([]byte, time.Duration, error) {
			started <- struct{}{}
			<-ctx.Done()
			return nil, 0, ctx.Err()
		}})
	cp.ProbeMgr.Start()
	t.Cleanup(cp.ProbeMgr.Stop)
	job, err := cp.StartSubscriptionProbe(sub.ID)
	if err != nil {
		t.Fatal(err)
	}
	select {
	case <-started:
	case <-time.After(time.Second):
		t.Fatal("probe not started")
	}
	if _, err := cp.CancelSubscriptionProbe(job.ID); err != nil {
		t.Fatal(err)
	}
	status := awaitSubscriptionProbe(t, cp)
	cp.ProbeMgr.Stop()
	if status.Status != "cancelled" || status.Failed != 0 || status.Completed != 0 {
		t.Fatalf("%+v", status)
	}
	if entry.FailureCount.Load() != 0 || entry.IsCircuitOpen() || entry.LastEgressUpdateAttempt.Load() != 0 {
		t.Fatal("cancelled probe changed node health")
	}
}

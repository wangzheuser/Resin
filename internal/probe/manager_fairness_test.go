package probe

import (
	"fmt"
	"testing"
	"time"

	"github.com/Resinat/Resin/internal/node"
	"github.com/Resinat/Resin/internal/topology"
)

// Capture the real map's iteration order so these regressions do not depend
// on its random hash seed or on insertion order.
func fairnessPool(t *testing.T, count int) (*topology.GlobalNodePool, []node.Hash) {
	t.Helper()
	pool := topology.NewGlobalNodePool(topology.PoolConfig{
		MaxLatencyTableEntries: 16,
		MaxConsecutiveFailures: func() int { return 2 },
	})
	for i := 0; i < count; i++ {
		raw := []byte(fmt.Sprintf(`{"type":"fairness-%d"}`, i))
		hash := node.HashFromRawOptions(raw)
		pool.AddNodeFromSub(hash, raw, "sub1")
		entry, _ := pool.GetEntry(hash)
		storeOutbound(entry)
	}
	var order []node.Hash
	pool.Range(func(hash node.Hash, _ *node.NodeEntry) bool {
		order = append(order, hash)
		return true
	})
	return pool, order
}

func takePeriodicTask(t *testing.T, mgr *ProbeManager) probeTask {
	t.Helper()
	mgr.taskQueue.mu.Lock()
	length := mgr.taskQueue.normal.len() + mgr.taskQueue.high.len()
	mgr.taskQueue.mu.Unlock()
	if length != 1 {
		t.Fatalf("queued probes = %d, want 1", length)
	}
	task, _ := mgr.taskQueue.Dequeue()
	state, ok := mgr.markTaskRunning(task.key)
	if !ok {
		t.Fatal("queued task was not runnable")
	}
	mgr.finishTask(task.key, state)
	return task
}

func TestPeriodicFairnessOldestDue(t *testing.T) {
	for _, kind := range []probeTaskKind{probeTaskKindEgress, probeTaskKindLatency} {
		t.Run(fmt.Sprintf("kind-%d", kind), func(t *testing.T) {
			pool, order := fairnessPool(t, 32)
			for _, hash := range order {
				entry, _ := pool.GetEntry(hash)
				entry.LastEgressUpdateAttempt.Store(time.Now().Add(-10 * time.Minute).UnixNano())
				entry.LastLatencyProbeAttempt.Store(time.Now().Add(-2 * time.Hour).UnixNano())
				if kind == probeTaskKindLatency {
					entry.CircuitOpenSince.Store(0)
				}
			}
			oldest := order[len(order)-1]
			entry, _ := pool.GetEntry(oldest)
			entry.LastEgressUpdateAttempt.Store(time.Now().Add(-48 * time.Hour).UnixNano())
			entry.LastLatencyProbeAttempt.Store(time.Now().Add(-48 * time.Hour).UnixNano())
			mgr := NewProbeManager(ProbeConfig{Pool: pool, Concurrency: 1})
			defer mgr.Stop()
			if kind == probeTaskKindEgress {
				mgr.scanEgress()
			} else {
				mgr.scanLatency()
			}
			if got := takePeriodicTask(t, mgr).key.hash; got != oldest {
				t.Fatal("oldest due node was starved by earlier map entries")
			}
		})
	}
}

func TestPeriodicFairnessEgressBalancesRecoveryAndRevalidation(t *testing.T) {
	pool, order := fairnessPool(t, 8)
	for i, hash := range order {
		entry, _ := pool.GetEntry(hash)
		entry.LastEgressUpdateAttempt.Store(time.Now().Add(-48 * time.Hour).UnixNano())
		if i >= 4 {
			entry.CircuitOpenSince.Store(0)
		}
	}
	mgr := NewProbeManager(ProbeConfig{Pool: pool, Concurrency: 1})
	defer mgr.Stop()
	seen := make(map[node.Hash]bool)
	for round := 0; round < 8; round++ {
		mgr.scanEgress()
		task := takePeriodicTask(t, mgr)
		entry, _ := pool.GetEntry(task.key.hash)
		if entry.IsCircuitOpen() != (round%2 == 1) {
			t.Fatalf("round %d: recovery and healthy revalidation must share the bounded budget", round)
		}
		if seen[task.key.hash] {
			t.Fatal("repeated probe before covering older due nodes")
		}
		seen[task.key.hash] = true
		// Model a completed probe that becomes due again before the backlog
		// drains. It must not jump ahead of untouched, older candidates.
		entry.LastEgressUpdateAttempt.Store(time.Now().Add(-25 * time.Hour).UnixNano())
	}
}

func TestPeriodicFairnessSkipsDuplicateWithoutConsumingBatch(t *testing.T) {
	for _, kind := range []probeTaskKind{probeTaskKindEgress, probeTaskKindLatency} {
		t.Run(fmt.Sprintf("kind-%d", kind), func(t *testing.T) {
			pool, order := fairnessPool(t, 3)
			for _, hash := range order {
				entry, _ := pool.GetEntry(hash)
				entry.CircuitOpenSince.Store(0)
			}
			mgr := NewProbeManager(ProbeConfig{Pool: pool, Concurrency: 1})
			defer mgr.Stop()
			mgr.enqueuePeriodicProbe(order[0], kind, probePriorityNormal)
			_, _ = mgr.taskQueue.Dequeue()
			state, ok := mgr.markTaskRunning(probeTaskKey{order[0], kind})
			if !ok {
				t.Fatal("task did not enter running state")
			}
			if kind == probeTaskKindEgress {
				mgr.scanEgress()
			} else {
				mgr.scanLatency()
			}
			if got := takePeriodicTask(t, mgr).key.hash; got == order[0] {
				t.Fatal("already running node consumed the new batch")
			}
			mgr.finishTask(probeTaskKey{order[0], kind}, state)
			if state.flags.Load() != 0 {
				t.Fatal("periodic duplicate scheduled a follow-up probe")
			}
		})
	}
}

func TestPeriodicFairnessFullQueuePreservesTurn(t *testing.T) {
	pool, order := fairnessPool(t, 3)
	healthy, _ := pool.GetEntry(order[2])
	healthy.CircuitOpenSince.Store(0)
	mgr := NewProbeManager(ProbeConfig{Pool: pool, Concurrency: 1, QueueCapacity: 1})
	defer mgr.Stop()
	mgr.enqueuePeriodicProbe(order[0], probeTaskKindEgress, probePriorityNormal)
	mgr.scanEgress()
	if mgr.periodicQueueFullEgress.Load() != 1 {
		t.Fatal("full queue was not recorded")
	}
	takePeriodicTask(t, mgr)
	mgr.scanEgress()
	if got := takePeriodicTask(t, mgr).key.hash; got != order[2] {
		t.Fatal("rejected enqueue consumed the healthy revalidation turn")
	}
}

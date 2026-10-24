package probe

import (
	"context"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/Resinat/Resin/internal/node"
	"github.com/Resinat/Resin/internal/topology"
)

func TestQueuedEgressSharesWorkerLimitAndStops(t *testing.T) {
	pool := topology.NewGlobalNodePool(topology.PoolConfig{MaxConsecutiveFailures: func() int { return 3 }})
	raw := []byte(`{"type":"queued"}`)
	hash := node.HashFromRawOptions(raw)
	pool.AddNodeFromSub(hash, raw, "sub")
	entry, _ := pool.GetEntry(hash)
	storeOutbound(entry)
	var active, peak atomic.Int32
	m := NewProbeManager(ProbeConfig{Pool: pool, Concurrency: 1,
		ContextFetcher: func(ctx context.Context, _ node.Hash, _ string) ([]byte, time.Duration, error) {
			n := active.Add(1)
			defer active.Add(-1)
			for old := peak.Load(); n > old; old = peak.Load() {
				if peak.CompareAndSwap(old, n) {
					break
				}
			}
			select {
			case <-ctx.Done():
				return nil, 0, ctx.Err()
			case <-time.After(30 * time.Millisecond):
			}
			return []byte("ip=203.0.113.2\n"), time.Millisecond, nil
		}})
	m.Start()
	t.Cleanup(m.Stop)
	var wg sync.WaitGroup
	for i := 0; i < 5; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if _, err := m.ProbeEgressQueued(context.Background(), hash); err != nil {
				t.Error(err)
			}
		}()
	}
	m.TriggerImmediateEgressProbe(hash)
	wg.Wait()
	m.Stop()
	if peak.Load() != 1 {
		t.Fatalf("exceeded shared worker limit: %d", peak.Load())
	}
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	if _, err := m.ProbeEgressQueued(ctx, hash); err == nil {
		t.Fatal("stopped manager accepted probe")
	}
}

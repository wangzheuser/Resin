package probe

import (
	"context"
	"net/netip"
	"testing"
	"time"

	"github.com/Resinat/Resin/internal/node"
	"github.com/Resinat/Resin/internal/topology"
)

func TestEgressInvalidTraceDoesNotRecoverNode(t *testing.T) {
	pool := topology.NewGlobalNodePool(topology.PoolConfig{MaxLatencyTableEntries: 16, MaxConsecutiveFailures: func() int { return 2 }})
	raw := []byte(`{"type":"egress-invalid"}`)
	hash := node.HashFromRawOptions(raw)
	pool.AddNodeFromSub(hash, raw, "sub1")
	entry, _ := pool.GetEntry(hash)
	storeOutbound(entry)
	oldIP := netip.MustParseAddr("203.0.113.7")
	entry.SetEgressIP(oldIP)
	pool.RecordResult(hash, false)
	pool.RecordResult(hash, false)
	body := []byte("<html>proxy error</html>")
	mgr := NewProbeManager(ProbeConfig{Pool: pool, Fetcher: func(node.Hash, string) ([]byte, time.Duration, error) {
		return body, time.Millisecond, nil
	}})
	_, stage, err := mgr.performEgressProbeContext(context.Background(), hash)
	if err == nil || stage != egressProbeParseError {
		t.Fatal("invalid trace must fail")
	}
	if !entry.IsCircuitOpen() || entry.FailureCount.Load() != 2 || entry.HasLatency() || entry.GetEgressIP() != oldIP {
		t.Fatalf("invalid trace recovered node or changed successful samples: circuit=%v failures=%d latency=%v", entry.IsCircuitOpen(), entry.FailureCount.Load(), entry.HasLatency())
	}
	body = []byte("ip=203.0.113.8\nloc=US\n")
	if _, _, err = mgr.performEgressProbeContext(context.Background(), hash); err != nil {
		t.Fatal(err)
	}
	if entry.IsCircuitOpen() || entry.FailureCount.Load() != 0 || !entry.HasLatency() || entry.GetEgressIP() == oldIP {
		t.Fatal("valid trace did not recover node")
	}
}

func TestEgressInvalidTraceDoesNotPenalizeHealthyNode(t *testing.T) {
	pool := topology.NewGlobalNodePool(topology.PoolConfig{MaxLatencyTableEntries: 16, MaxConsecutiveFailures: func() int { return 2 }})
	raw := []byte(`{"type":"egress-soft-failure"}`)
	hash := node.HashFromRawOptions(raw)
	pool.AddNodeFromSub(hash, raw, "sub1")
	entry, _ := pool.GetEntry(hash)
	storeOutbound(entry)
	entry.SetEgressIP(netip.MustParseAddr("203.0.113.7"))
	pool.RecordResult(hash, true)

	mgr := NewProbeManager(ProbeConfig{Pool: pool, Fetcher: func(node.Hash, string) ([]byte, time.Duration, error) {
		return []byte("<html>blocked</html>"), time.Millisecond, nil
	}})
	if _, stage, err := mgr.performEgressProbeContext(context.Background(), hash); err == nil || stage != egressProbeParseError {
		t.Fatalf("expected invalid trace failure, stage=%v err=%v", stage, err)
	}
	if entry.FailureCount.Load() != 0 || entry.IsCircuitOpen() || entry.GetEgressIP().String() != "203.0.113.7" {
		t.Fatalf("invalid trace should not penalize node: failures=%d circuit=%v ip=%s", entry.FailureCount.Load(), entry.IsCircuitOpen(), entry.GetEgressIP())
	}
}

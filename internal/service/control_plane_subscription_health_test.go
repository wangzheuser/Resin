package service

import (
	"encoding/json"
	"fmt"
	"testing"
	"time"

	"github.com/Resinat/Resin/internal/node"
	"github.com/Resinat/Resin/internal/subscription"
	"github.com/Resinat/Resin/internal/testutil"
)

func TestSubscriptionHealthBreakdown(t *testing.T) {
	cp, mgr, pool := newCleanupSubscriptionTestService()
	sub := subscription.NewSubscription("health-sub", "health", "https://example.com", true, false)
	mgr.Register(sub)
	for i, kind := range []string{"healthy", "pending", "build_failed", "failed", "building", "missing", "evicted"} {
		raw := []byte(fmt.Sprintf(`{"type":"test","id":%d}`, i))
		hash := node.HashFromRawOptions(raw)
		sub.ManagedNodes().StoreNode(hash, subscription.ManagedNode{Evicted: kind == "evicted"})
		if kind == "missing" || kind == "evicted" {
			continue
		}
		pool.AddNodeFromSub(hash, raw, sub.ID)
		entry, _ := pool.GetEntry(hash)
		if kind != "building" && kind != "build_failed" {
			ob := testutil.NewNoopOutbound()
			entry.Outbound.Store(&ob)
		}
		switch kind {
		case "healthy":
			entry.CircuitOpenSince.Store(0)
			entry.LastEgressUpdateAttempt.Store(time.Now().UnixNano())
		case "failed":
			entry.FailureCount.Store(2)
			entry.LastEgressUpdateAttempt.Store(time.Now().Add(-25 * time.Hour).UnixNano())
		case "build_failed":
			entry.SetLastError("outbound build: invalid fingerprint")
		}
	}
	assertCounts := func(want map[string]int) map[string]any {
		t.Helper()
		data, err := json.Marshal(cp.subToResponse(sub))
		if err != nil {
			t.Fatal(err)
		}
		var got map[string]any
		if err := json.Unmarshal(data, &got); err != nil {
			t.Fatal(err)
		}
		for key, value := range want {
			if got[key] != float64(value) {
				t.Errorf("%s = %v, want %d", key, got[key], value)
			}
		}
		return got
	}
	got := assertCounts(map[string]int{"node_count": 6, "healthy_node_count": 1, "pending_node_count": 3,
		"build_failed_node_count": 1, "failed_node_count": 1, "disabled_node_count": 0, "stale_node_count": 1})
	if stamp, ok := got["last_probe_at"].(string); !ok || stamp == "" {
		t.Error("last_probe_at missing")
	}
	sub.SetEnabled(false)
	assertCounts(map[string]int{"node_count": 6, "healthy_node_count": 0, "pending_node_count": 0,
		"build_failed_node_count": 0, "failed_node_count": 0, "disabled_node_count": 6})
}

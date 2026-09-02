package routing

import (
	"bytes"
	"log"
	"net/netip"
	"strings"
	"testing"
	"time"

	"github.com/Resinat/Resin/internal/model"
	"github.com/Resinat/Resin/internal/platform"
)

func TestTargetEgressCircuitTracksDomainAndExpires(t *testing.T) {
	now := time.Unix(1_000, 0)
	tracker := newTargetEgressCircuitTracker(func() time.Time { return now }, 16)
	ip := netip.MustParseAddr("203.0.113.10")

	if tracker.recordFailure("plat", ip, "llm.atxp.ai:443", "timeout") {
		t.Fatal("first failure must not open the circuit")
	}
	now = now.Add(10 * time.Second)
	if !tracker.recordFailure("plat", ip, "https://llm.atxp.ai/v1", "eof") {
		t.Fatal("second failure inside the window must open the circuit")
	}
	if !tracker.isOpen("plat", ip, "llm.atxp.ai:443") {
		t.Fatal("ATXP circuit should be open")
	}
	if tracker.isOpen("plat", ip, "api.example.com:443") {
		t.Fatal("another target domain must remain available")
	}

	tracker.recordSuccess("plat", ip, "api.example.com:443")
	if !tracker.isOpen("plat", ip, "llm.atxp.ai:443") {
		t.Fatal("success for another domain must not reset ATXP")
	}
	tracker.recordSuccess("plat", ip, "llm.atxp.ai:443")
	if tracker.isOpen("plat", ip, "llm.atxp.ai:443") {
		t.Fatal("success for the same domain must reset its circuit")
	}

	tracker.recordFailure("plat", ip, "llm.atxp.ai:443", "timeout")
	tracker.recordFailure("plat", ip, "llm.atxp.ai:443", "timeout")
	now = now.Add(targetEgressOpenDuration + time.Second)
	if tracker.isOpen("plat", ip, "llm.atxp.ai:443") {
		t.Fatal("expired circuit must be selectable again")
	}
}

func TestTargetEgressCircuitCleanupIsBounded(t *testing.T) {
	now := time.Unix(2_000, 0)
	tracker := newTargetEgressCircuitTracker(func() time.Time { return now }, 2)
	for _, raw := range []string{"203.0.113.1", "203.0.113.2", "203.0.113.3"} {
		tracker.recordFailure("plat", netip.MustParseAddr(raw), "llm.atxp.ai", "timeout")
		now = now.Add(time.Second)
	}
	if got := tracker.entryCount(); got != 2 {
		t.Fatalf("entry count = %d, want 2", got)
	}
	now = now.Add(targetEgressEntryTTL + time.Second)
	tracker.cleanup()
	if got := tracker.entryCount(); got != 0 {
		t.Fatalf("expired entry count = %d, want 0", got)
	}
}

func TestRouteRequestExcludingReplacesStickyEgressAndCASDeleteProtectsNewLease(t *testing.T) {
	pool := newRouterTestPool()
	plat := newTestPlatform("plat-exclude", "Plat-Exclude")
	pool.addPlatform(plat)

	firstHash, firstEntry := newRoutableEntry(t, `{"id":"first"}`, "203.0.113.20")
	secondHash, secondEntry := newRoutableEntry(t, `{"id":"second"}`, "203.0.113.21")
	pool.addEntry(firstHash, firstEntry)
	pool.addEntry(secondHash, secondEntry)
	pool.rebuildPlatformView(plat)
	router := newTestRouter(pool, nil)

	now := time.Now()
	if err := router.UpsertLease(model.Lease{
		PlatformID:     plat.ID,
		Account:        "acct",
		NodeHash:       firstHash.Hex(),
		EgressIP:       firstEntry.GetEgressIP().String(),
		CreatedAtNs:    now.UnixNano(),
		ExpiryNs:       now.Add(time.Hour).UnixNano(),
		LastAccessedNs: now.UnixNano(),
	}); err != nil {
		t.Fatalf("upsert first lease: %v", err)
	}

	if !router.DeleteLeaseIfMatch(plat.ID, "acct", firstHash, firstEntry.GetEgressIP()) {
		t.Fatal("matching lease should be deleted")
	}
	result, err := router.RouteRequestExcluding(
		plat.Name,
		"acct",
		"llm.atxp.ai:443",
		map[netip.Addr]struct{}{firstEntry.GetEgressIP(): {}},
	)
	if err != nil {
		t.Fatalf("route excluding failed egress: %v", err)
	}
	if result.NodeHash != secondHash || result.EgressIP != secondEntry.GetEgressIP() {
		t.Fatalf("replacement route = %s/%s, want %s/%s", result.NodeHash.Hex(), result.EgressIP, secondHash.Hex(), secondEntry.GetEgressIP())
	}
	if router.DeleteLeaseIfMatch(plat.ID, "acct", firstHash, firstEntry.GetEgressIP()) {
		t.Fatal("stale failure callback must not delete the replacement lease")
	}
}

func TestRouteRequestSkipsTargetCircuitOpenEgress(t *testing.T) {
	pool := newRouterTestPool()
	plat := newTestPlatform("plat-circuit", "Plat-Circuit")
	pool.addPlatform(plat)
	badHash, badEntry := newRoutableEntry(t, `{"id":"bad"}`, "203.0.113.30")
	goodHash, goodEntry := newRoutableEntry(t, `{"id":"good"}`, "203.0.113.31")
	pool.addEntry(badHash, badEntry)
	pool.addEntry(goodHash, goodEntry)
	pool.rebuildPlatformView(plat)
	router := newTestRouter(pool, nil)
	badRoute := RouteResult{
		PlatformID: plat.ID,
		NodeHash:   badHash,
		EgressIP:   badEntry.GetEgressIP(),
	}
	router.RecordTargetEgressFailure(badRoute, "llm.atxp.ai:443", "timeout")
	router.RecordTargetEgressFailure(badRoute, "llm.atxp.ai:443", "timeout")

	for i := 0; i < 20; i++ {
		result, err := router.RouteRequest(plat.Name, "", "llm.atxp.ai:443")
		if err != nil {
			t.Fatalf("route %d: %v", i, err)
		}
		if result.NodeHash != goodHash || result.EgressIP != goodEntry.GetEgressIP() {
			t.Fatalf("route selected circuit-open egress: %+v", result)
		}
	}
}

func TestTargetEgressCircuitLogDoesNotExposeRawIP(t *testing.T) {
	now := time.Unix(3_000, 0)
	tracker := newTargetEgressCircuitTracker(func() time.Time { return now }, 16)
	ip := netip.MustParseAddr("203.0.113.88")
	var output bytes.Buffer
	previous := log.Writer()
	log.SetOutput(&output)
	t.Cleanup(func() { log.SetOutput(previous) })

	tracker.recordFailure("plat", ip, "llm.atxp.ai", "timeout")
	tracker.recordFailure("plat", ip, "llm.atxp.ai", "timeout")
	if strings.Contains(output.String(), ip.String()) {
		t.Fatalf("log leaked raw egress IP: %s", output.String())
	}
	if !strings.Contains(output.String(), "egress_hash=") {
		t.Fatalf("log did not include egress hash: %s", output.String())
	}
}

func newTestPlatform(id, name string) *platform.Platform {
	plat := platform.NewPlatform(id, name, nil, nil)
	plat.StickyTTLNs = int64(time.Hour)
	return plat
}

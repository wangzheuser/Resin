package proxy

import (
	"context"
	"encoding/json"
	"errors"
	"net"
	"net/netip"
	"sync/atomic"
	"testing"
	"time"

	"github.com/Resinat/Resin/internal/model"
	"github.com/Resinat/Resin/internal/node"
	"github.com/Resinat/Resin/internal/routing"
	"github.com/Resinat/Resin/internal/subscription"
	"github.com/sagernet/sing-box/adapter"
	M "github.com/sagernet/sing/common/metadata"
)

func addFailoverNode(
	t *testing.T,
	env *proxyE2EEnv,
	raw string,
	ip string,
	dial func(context.Context, string, M.Socksaddr) (net.Conn, error),
) (node.Hash, *node.NodeEntry) {
	t.Helper()
	rawOptions := json.RawMessage(raw)
	hash := node.HashFromRawOptions(rawOptions)
	sub, ok := env.subMgr.Get("sub-1")
	if !ok {
		t.Fatal("test subscription not found")
	}
	sub.ManagedNodes().StoreNode(hash, subscription.ManagedNode{Tags: []string{"tag"}})
	env.pool.AddNodeFromSub(hash, rawOptions, sub.ID)
	entry, ok := env.pool.GetEntry(hash)
	if !ok {
		t.Fatal("test node not found")
	}
	var outbound adapter.Outbound = &mockOutbound{dialFunc: dial}
	entry.Outbound.Store(&outbound)
	entry.SetEgressIP(netip.MustParseAddr(ip))
	entry.LatencyTable.Update("atxp.ai", 20*time.Millisecond, 10*time.Minute)
	env.pool.RecordResult(hash, true)
	env.pool.NotifyNodeDirty(hash)
	return hash, entry
}

func installStickyLease(
	t *testing.T,
	router *routing.Router,
	account string,
	hash node.Hash,
	ip netip.Addr,
) {
	t.Helper()
	now := time.Now()
	if err := router.UpsertLease(model.Lease{
		PlatformID:     "plat-id",
		Account:        account,
		NodeHash:       hash.Hex(),
		EgressIP:       ip.String(),
		CreatedAtNs:    now.UnixNano(),
		ExpiryNs:       now.Add(time.Hour).UnixNano(),
		LastAccessedNs: now.UnixNano(),
	}); err != nil {
		t.Fatalf("install sticky lease: %v", err)
	}
}

func TestPrepareConnectTunnelRetriesOnceWithDifferentEgress(t *testing.T) {
	env := newProxyE2EEnv(t)
	firstHash := node.HashFromRawOptions(json.RawMessage(`{"type":"stub","server":"127.0.0.1","server_port":1}`))
	firstEntry, ok := env.pool.GetEntry(firstHash)
	if !ok {
		t.Fatal("initial node not found")
	}
	var firstCalls atomic.Int32
	var firstOutbound adapter.Outbound = &mockOutbound{dialFunc: func(context.Context, string, M.Socksaddr) (net.Conn, error) {
		firstCalls.Add(1)
		return nil, errors.New("connection refused")
	}}
	firstEntry.Outbound.Store(&firstOutbound)

	var secondCalls atomic.Int32
	var peer net.Conn
	secondHash, secondEntry := addFailoverNode(
		t,
		env,
		`{"type":"stub","server":"127.0.0.2","server_port":2}`,
		"203.0.113.11",
		func(context.Context, string, M.Socksaddr) (net.Conn, error) {
			secondCalls.Add(1)
			conn, other := net.Pipe()
			peer = other
			return conn, nil
		},
	)
	defer func() {
		if peer != nil {
			_ = peer.Close()
		}
	}()
	installStickyLease(t, env.router, "acct", firstHash, firstEntry.GetEgressIP())

	result := prepareConnectTunnel(
		context.Background(),
		tunnelDeps{router: env.router, pool: env.pool},
		"plat",
		"acct",
		"llm.atxp.ai:443",
	)
	if result.session == nil {
		t.Fatalf("failover did not recover: proxyErr=%v upstreamErr=%v", result.proxyErr, result.upstreamErr)
	}
	defer result.session.upstreamConn.Close()
	if firstCalls.Load() != 1 || secondCalls.Load() != 1 {
		t.Fatalf("dial calls = %d/%d, want 1/1", firstCalls.Load(), secondCalls.Load())
	}
	if result.route.NodeHash != secondHash || result.route.EgressIP != secondEntry.GetEgressIP() {
		t.Fatalf("final route = %s/%s, want %s/%s", result.route.NodeHash.Hex(), result.route.EgressIP, secondHash.Hex(), secondEntry.GetEgressIP())
	}
	stats := env.router.TargetEgressStats()
	if stats.FailoverAttempts != 1 || stats.FailoverSuccesses != 1 || stats.FailoverFailures != 0 {
		t.Fatalf("unexpected failover stats: %+v", stats)
	}
}

func TestPrepareConnectTunnelStopsAfterSecondDialFailure(t *testing.T) {
	env := newProxyE2EEnv(t)
	firstHash := node.HashFromRawOptions(json.RawMessage(`{"type":"stub","server":"127.0.0.1","server_port":1}`))
	firstEntry, ok := env.pool.GetEntry(firstHash)
	if !ok {
		t.Fatal("initial node not found")
	}
	var calls atomic.Int32
	failingDial := func(context.Context, string, M.Socksaddr) (net.Conn, error) {
		calls.Add(1)
		return nil, errors.New("unexpected EOF")
	}
	var firstOutbound adapter.Outbound = &mockOutbound{dialFunc: failingDial}
	firstEntry.Outbound.Store(&firstOutbound)
	addFailoverNode(
		t,
		env,
		`{"type":"stub","server":"127.0.0.3","server_port":3}`,
		"203.0.113.12",
		failingDial,
	)
	installStickyLease(t, env.router, "acct-two", firstHash, firstEntry.GetEgressIP())

	result := prepareConnectTunnel(
		context.Background(),
		tunnelDeps{router: env.router, pool: env.pool},
		"plat",
		"acct-two",
		"llm.atxp.ai:443",
	)
	if result.session != nil || result.proxyErr == nil {
		t.Fatalf("second failure should terminate: %+v", result)
	}
	if calls.Load() != 2 {
		t.Fatalf("dial calls = %d, want exactly 2", calls.Load())
	}
	stats := env.router.TargetEgressStats()
	if stats.FailoverAttempts != 1 || stats.FailoverFailures != 1 {
		t.Fatalf("unexpected failover stats: %+v", stats)
	}
}

func TestPostCommitFailureInvalidatesLeaseWithoutReplay(t *testing.T) {
	env := newProxyE2EEnv(t)
	firstHash := node.HashFromRawOptions(json.RawMessage(`{"type":"stub","server":"127.0.0.1","server_port":1}`))
	firstEntry, ok := env.pool.GetEntry(firstHash)
	if !ok {
		t.Fatal("initial node not found")
	}
	var calls atomic.Int32
	var peer net.Conn
	var firstOutbound adapter.Outbound = &mockOutbound{dialFunc: func(context.Context, string, M.Socksaddr) (net.Conn, error) {
		calls.Add(1)
		conn, other := net.Pipe()
		peer = other
		return conn, nil
	}}
	firstEntry.Outbound.Store(&firstOutbound)
	secondHash, secondEntry := addFailoverNode(
		t,
		env,
		`{"type":"stub","server":"127.0.0.4","server_port":4}`,
		"203.0.113.14",
		func(context.Context, string, M.Socksaddr) (net.Conn, error) {
			return nil, errors.New("unused")
		},
	)
	installStickyLease(t, env.router, "acct-post", firstHash, firstEntry.GetEgressIP())

	result := prepareConnectTunnel(
		context.Background(),
		tunnelDeps{router: env.router, pool: env.pool},
		"plat",
		"acct-post",
		"llm.atxp.ai:443",
	)
	if result.session == nil {
		t.Fatalf("prepare failed: %v", result.proxyErr)
	}
	result.session.recordResult(false, "eof", true)
	if lease := env.router.ReadLease(model.LeaseKey{PlatformID: "plat-id", Account: "acct-post"}); lease == nil {
		t.Fatal("first post-commit failure must retain the sticky lease")
	}
	result.session.recordResult(false, "eof", true)
	_ = result.session.upstreamConn.Close()
	if peer != nil {
		_ = peer.Close()
	}
	if calls.Load() != 1 {
		t.Fatalf("post-commit failure replayed the request: calls=%d", calls.Load())
	}
	if lease := env.router.ReadLease(model.LeaseKey{PlatformID: "plat-id", Account: "acct-post"}); lease != nil {
		t.Fatalf("failed post-commit lease was not invalidated: %+v", lease)
	}
	if stats := env.router.TargetEgressStats(); stats.FailoverAttempts != 0 {
		t.Fatalf("post-commit path must not count a pre-commit retry: %+v", stats)
	}
	next, err := env.router.RouteRequest("plat", "acct-post", "llm.atxp.ai:443")
	if err != nil {
		t.Fatalf("route after post-commit failure: %v", err)
	}
	if next.NodeHash != secondHash || next.EgressIP != secondEntry.GetEgressIP() {
		t.Fatalf("next route reused failed egress: %+v", next)
	}
}

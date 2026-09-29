package proxy

import (
	"context"
	"encoding/json"
	"errors"
	"net"
	"testing"
	"time"

	"github.com/Resinat/Resin/internal/node"
	"github.com/sagernet/sing-box/adapter"
	M "github.com/sagernet/sing/common/metadata"
)

func TestPrepareConnectTunnelHasDialDeadline(t *testing.T) {
	env := newProxyE2EEnv(t)
	hash := node.HashFromRawOptions(json.RawMessage(`{"type":"stub","server":"127.0.0.1","server_port":1}`))
	entry, _ := env.pool.GetEntry(hash)
	var ob adapter.Outbound = &mockOutbound{dialFunc: func(ctx context.Context, _ string, _ M.Socksaddr) (net.Conn, error) {
		if deadline, ok := ctx.Deadline(); !ok || time.Until(deadline) > 16*time.Second {
			t.Error("CONNECT dial has no bounded deadline")
		}
		return nil, errors.New("test dial failed")
	}}
	entry.Outbound.Store(&ob)
	prepareConnectTunnel(context.Background(), tunnelDeps{router: env.router, pool: env.pool}, "plat", "acct", "example.com:443")
}

type stalledEarlyConn struct{ net.Conn }

func (*stalledEarlyConn) NeedHandshake() bool         { return true }
func (c *stalledEarlyConn) Write([]byte) (int, error) { return c.Conn.Write([]byte("handshake")) }

func TestPrepareConnectTunnelEarlyHandshakeDeadline(t *testing.T) {
	env := newProxyE2EEnv(t)
	hash := node.HashFromRawOptions(json.RawMessage(`{"type":"stub","server":"127.0.0.1","server_port":1}`))
	entry, _ := env.pool.GetEntry(hash)
	conn, peer := net.Pipe()
	defer conn.Close()
	defer peer.Close()
	// Bound the unpatched test itself so a regression fails instead of hanging.
	_ = conn.SetWriteDeadline(time.Now().Add(500 * time.Millisecond))
	var ob adapter.Outbound = &mockOutbound{dialFunc: func(context.Context, string, M.Socksaddr) (net.Conn, error) {
		return &stalledEarlyConn{Conn: conn}, nil
	}}
	entry.Outbound.Store(&ob)
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Millisecond)
	defer cancel()
	started := time.Now()
	result := prepareConnectTunnel(ctx, tunnelDeps{router: env.router, pool: env.pool}, "plat", "acct", "example.com:443")
	if result.session != nil {
		result.session.upstreamConn.Close()
		t.Fatal("stalled handshake succeeded")
	}
	if elapsed := time.Since(started); elapsed > 250*time.Millisecond {
		t.Fatalf("handshake ignored cancellation: %s", elapsed)
	}
}

func TestPrepareConnectTunnelExpiredBudgetDoesNotPenalizeNextNode(t *testing.T) {
	env := newProxyE2EEnv(t)
	hash := node.HashFromRawOptions(json.RawMessage(`{"type":"stub","server":"127.0.0.1","server_port":1}`))
	entry, _ := env.pool.GetEntry(hash)
	var first adapter.Outbound = &mockOutbound{dialFunc: func(ctx context.Context, _ string, _ M.Socksaddr) (net.Conn, error) {
		<-ctx.Done()
		return nil, ctx.Err()
	}}
	entry.Outbound.Store(&first)
	secondCalls := 0
	addFailoverNode(t, env, `{"type":"stub","server":"127.0.0.2","server_port":2}`, "203.0.113.11",
		func(ctx context.Context, _ string, _ M.Socksaddr) (net.Conn, error) {
			secondCalls++
			return nil, ctx.Err()
		})
	installStickyLease(t, env.router, "deadline", hash, entry.GetEgressIP())
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Millisecond)
	defer cancel()
	result := prepareConnectTunnel(ctx, tunnelDeps{router: env.router, pool: env.pool}, "plat", "deadline", "llm.atxp.ai:443")
	if result.session != nil || !errors.Is(result.upstreamErr, context.DeadlineExceeded) {
		t.Fatalf("expected setup deadline error: %+v", result)
	}
	if secondCalls != 0 || result.route.NodeHash != hash {
		t.Fatalf("expired setup dialed or blamed another node: calls=%d, route=%s", secondCalls, result.route.NodeHash.Hex())
	}
	if stats := env.router.TargetEgressStats(); stats.FailoverAttempts != 0 {
		t.Fatalf("expired setup counted an impossible failover: %+v", stats)
	}
}

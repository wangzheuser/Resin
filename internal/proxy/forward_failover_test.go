package proxy

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"net/netip"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/Resinat/Resin/internal/model"
	"github.com/Resinat/Resin/internal/node"
	"github.com/sagernet/sing-box/adapter"
	M "github.com/sagernet/sing/common/metadata"
)

func TestForwardProxyRetriesSafeHTTPOnDialFailure(t *testing.T) {
	env := newProxyE2EEnv(t)
	firstRaw := json.RawMessage(`{"type":"stub","server":"127.0.0.1","server_port":1}`)
	firstHash := node.HashFromRawOptions(firstRaw)
	firstEntry, ok := env.pool.GetEntry(firstHash)
	if !ok {
		t.Fatal("initial node not found")
	}
	var firstCalls atomic.Int32
	var firstOutbound adapter.Outbound = &mockOutbound{dialFunc: func(context.Context, string, M.Socksaddr) (net.Conn, error) {
		firstCalls.Add(1)
		return nil, errors.New("simulated dial failure")
	}}
	firstEntry.Outbound.Store(&firstOutbound)

	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusNoContent)
	}))
	defer upstream.Close()

	secondHash, _ := addFailoverNode(t, env, `{"type":"stub","server":"127.0.0.2","server_port":2}`, "203.0.113.11",
		func(ctx context.Context, network string, dest M.Socksaddr) (net.Conn, error) {
			return (&net.Dialer{}).DialContext(ctx, network, dest.String())
		})
	installStickyLease(t, env.router, "acct", firstHash, netip.MustParseAddr("203.0.113.10"))

	fp := NewForwardProxy(ForwardProxyConfig{
		ProxyToken: "tok",
		Router:     env.router,
		Pool:       env.pool,
		Health:     env.pool,
		Events:     NoOpEventEmitter{},
	})
	req := httptest.NewRequest(http.MethodGet, upstream.URL+"/generate_204", nil)
	req.Header.Set("Proxy-Authorization", basicAuth("tok", "plat:acct"))
	w := httptest.NewRecorder()
	fp.ServeHTTP(w, req)

	if w.Code != http.StatusNoContent {
		t.Fatalf("status=%d body=%q resin_error=%q", w.Code, w.Body.String(), w.Header().Get("X-Resin-Error"))
	}
	if firstCalls.Load() != 1 {
		t.Fatalf("first node dial calls=%d, want 1", firstCalls.Load())
	}
	if env.router.ReadLease(model.LeaseKey{PlatformID: "plat-id", Account: "acct"}) == nil {
		t.Fatal("lease should remain readable after safe retry")
	}
	if route := env.router.ReadLease(model.LeaseKey{PlatformID: "plat-id", Account: "acct"}); route.NodeHash != secondHash.Hex() {
		t.Fatalf("lease route=%s, want retry node %s", route.NodeHash, secondHash.Hex())
	}
	waitForNodeFailureCount(t, firstEntry, 1)
	if stats := env.router.TargetEgressStats(); stats.FailoverAttempts != 1 || stats.FailoverSuccesses != 1 {
		t.Fatalf("failover stats: %+v", stats)
	}
}

func TestForwardFailoverDoesNotReplayWrittenOrBodyRequests(t *testing.T) {
	for _, tc := range []struct {
		name, method, body string
		written            bool
	}{
		{"post-before-dial", http.MethodPost, "", false},
		{"get-with-body", http.MethodGet, "payload", false},
		{"get-after-write", http.MethodGet, "", true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			env := newProxyE2EEnv(t)
			firstHash := node.HashFromRawOptions(json.RawMessage(`{"type":"stub","server":"127.0.0.1","server_port":1}`))
			entry, _ := env.pool.GetEntry(firstHash)
			setProxyE2EOutboundDialFunc(t, env, func(context.Context, string, M.Socksaddr) (net.Conn, error) {
				if !tc.written {
					return nil, errors.New("dial failed")
				}
				conn, peer := net.Pipe()
				go func() {
					defer peer.Close()
					_, _ = http.ReadRequest(bufio.NewReader(peer))
				}()
				return conn, nil
			})
			var secondCalls atomic.Int32
			addFailoverNode(t, env, `{"type":"stub","server":"127.0.0.2","server_port":2}`, "203.0.113.11", func(context.Context, string, M.Socksaddr) (net.Conn, error) {
				secondCalls.Add(1)
				return nil, errors.New("must not retry")
			})
			installStickyLease(t, env.router, "acct", firstHash, entry.GetEgressIP())
			fp := NewForwardProxy(ForwardProxyConfig{ProxyToken: "tok", Router: env.router, Pool: env.pool, Events: NoOpEventEmitter{}})
			defer fp.transportPool.CloseAll()
			var body io.Reader
			if tc.body != "" {
				body = strings.NewReader(tc.body)
			}
			req := httptest.NewRequest(tc.method, "http://example.com/", body)
			req.Header.Set("Proxy-Authorization", basicAuth("tok", "plat:acct"))
			w := httptest.NewRecorder()
			fp.ServeHTTP(w, req)
			if w.Code != http.StatusBadGateway || secondCalls.Load() != 0 {
				t.Fatalf("status=%d replacement dials=%d", w.Code, secondCalls.Load())
			}
		})
	}
}

func TestForwardFailoverPreservesStreamingBody(t *testing.T) {
	env := newProxyE2EEnv(t)
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
		w.(http.Flusher).Flush()
		time.Sleep(100 * time.Millisecond)
		_, _ = io.WriteString(w, "complete delayed body")
	}))
	defer upstream.Close()
	fp := NewForwardProxy(ForwardProxyConfig{ProxyToken: "tok", Router: env.router, Pool: env.pool, Events: NoOpEventEmitter{}})
	defer fp.transportPool.CloseAll()
	req := httptest.NewRequest(http.MethodGet, upstream.URL, nil)
	req.Header.Set("Proxy-Authorization", basicAuth("tok", "plat"))
	w := httptest.NewRecorder()
	fp.ServeHTTP(w, req)
	if w.Code != http.StatusOK || w.Body.String() != "complete delayed body" {
		t.Fatalf("stream was truncated: status=%d body=%q", w.Code, w.Body.String())
	}
}

func TestConnectTimeoutReservesFailoverBudget(t *testing.T) {
	env := newProxyE2EEnv(t)
	firstHash := node.HashFromRawOptions(json.RawMessage(`{"type":"stub","server":"127.0.0.1","server_port":1}`))
	first, _ := env.pool.GetEntry(firstHash)
	setProxyE2EOutboundDialFunc(t, env, func(ctx context.Context, _ string, _ M.Socksaddr) (net.Conn, error) {
		<-ctx.Done()
		return nil, ctx.Err()
	})
	conn, peer := net.Pipe()
	defer conn.Close()
	defer peer.Close()
	secondHash, _ := addFailoverNode(t, env, `{"type":"stub","server":"127.0.0.2","server_port":2}`, "203.0.113.11", func(ctx context.Context, _ string, _ M.Socksaddr) (net.Conn, error) {
		if ctx.Err() != nil {
			return nil, ctx.Err()
		}
		return conn, nil
	})
	installStickyLease(t, env.router, "acct", firstHash, first.GetEgressIP())
	ctx, cancel := context.WithTimeout(context.Background(), 9*time.Second)
	defer cancel()
	result := prepareConnectTunnel(ctx, tunnelDeps{router: env.router, pool: env.pool, health: env.pool}, "plat", "acct", "example.com:443")
	if result.session == nil || result.route.NodeHash != secondHash {
		t.Fatalf("first timeout consumed failover budget: session=%v route=%s error=%v", result.session != nil, result.route.NodeHash.Hex(), result.upstreamErr)
	}
	waitForNodeFailureCount(t, first, 1)
}

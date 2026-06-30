package proxy

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"net/http"
	"net/http/httptest"
	"net/netip"
	"sort"
	"strings"
	"testing"
	"time"

	"github.com/Resinat/Resin/internal/model"
	"github.com/Resinat/Resin/internal/node"
	"github.com/Resinat/Resin/internal/subscription"
	"github.com/Resinat/Resin/internal/testutil"
	M "github.com/sagernet/sing/common/metadata"
)

func newTestProxySessionManager(t *testing.T, env *proxyE2EEnv, minPort, maxPort int) *ProxySessionManager {
	t.Helper()
	mgr := NewProxySessionManager(ProxySessionManagerConfig{
		Router:            env.router,
		Pool:              env.pool,
		Health:            &mockHealthRecorder{},
		Events:            newMockEventEmitter(),
		TransportPool:     NewOutboundTransportPool(OutboundTransportConfig{}),
		MinPort:           minPort,
		MaxPort:           maxPort,
		MaxActiveSessions: 1000,
		PortRetries:       128,
	})
	t.Cleanup(mgr.Close)
	return mgr
}

func addProxySessionTestNode(t *testing.T, env *proxyE2EEnv, raw string, ip string) node.Hash {
	t.Helper()
	hash := node.HashFromRawOptions([]byte(raw))
	entry := node.NewNodeEntry(hash, []byte(raw), time.Now(), 16)
	entry.SetEgressIP(netip.MustParseAddr(ip))
	entry.LatencyTable.Update("example.com", 20*time.Millisecond, 10*time.Minute)
	ob := testutil.NewNoopOutbound()
	entry.Outbound.Store(&ob)
	if sub := env.subMgr.Lookup("sub-1"); sub != nil {
		sub.ManagedNodes().StoreNode(hash, subscription.ManagedNode{Tags: []string{"tag"}})
	}
	env.pool.LoadNodeFromBootstrap(entry)
	env.pool.AddNodeFromSub(hash, json.RawMessage(raw), "sub-1")
	plat, ok := env.pool.GetPlatformByName("plat")
	if !ok {
		t.Fatal("platform not found")
	}
	if !plat.View().Contains(hash) {
		t.Fatalf("node %s should be routable", hash.Hex())
	}
	return hash
}

func TestProxySessionManager_RoundRobinReuse(t *testing.T) {
	env := newProxyE2EEnv(t)
	mgr := newTestProxySessionManager(t, env, 21000, 22000)

	first := node.HashFromRawOptions([]byte(`{"type":"stub","server":"127.0.0.1","server_port":1}`))
	second := addProxySessionTestNode(t, env, `{"type":"stub","server":"127.0.0.2","server_port":2}`, "203.0.113.20")
	third := addProxySessionTestNode(t, env, `{"type":"stub","server":"127.0.0.3","server_port":3}`, "203.0.113.30")
	sorted := []string{first.Hex(), second.Hex(), third.Hex()}
	sort.Strings(sorted)

	got := make([]string, 0, 5)
	for i := 0; i < 5; i++ {
		info, err := mgr.Create(ProxySessionCreateRequest{PlatformName: "plat", TTL: time.Minute})
		if err != nil {
			t.Fatalf("Create(%d): %v", i, err)
		}
		got = append(got, info.NodeHash)
	}
	want := []string{sorted[0], sorted[1], sorted[2], sorted[0], sorted[1]}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("round robin[%d]: got %s, want %s; all=%v", i, got[i], want[i], got)
		}
	}
}

func TestProxySessionManager_ReleaseAndTTLDeleteLease(t *testing.T) {
	env := newProxyE2EEnv(t)
	mgr := newTestProxySessionManager(t, env, 22001, 22100)

	info, err := mgr.Create(ProxySessionCreateRequest{PlatformName: "plat", TTL: time.Minute})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	lease := env.router.ReadLease(model.LeaseKey{PlatformID: info.PlatformID, Account: info.Account})
	if lease == nil {
		t.Fatal("expected lease after create")
	}
	if err := mgr.Release("plat", info.ID); err != nil {
		t.Fatalf("Release: %v", err)
	}
	lease = env.router.ReadLease(model.LeaseKey{PlatformID: info.PlatformID, Account: info.Account})
	if lease != nil {
		t.Fatal("expected lease to be deleted after release")
	}
	if err := mgr.Release("plat", info.ID); !errors.Is(err, ErrProxySessionNotFound) {
		t.Fatalf("second release: got %v, want ErrProxySessionNotFound", err)
	}

	ttlInfo, err := mgr.Create(ProxySessionCreateRequest{PlatformName: "plat", TTL: 20 * time.Millisecond})
	if err != nil {
		t.Fatalf("Create ttl: %v", err)
	}
	deadline := time.Now().Add(time.Second)
	for time.Now().Before(deadline) {
		if env.router.ReadLease(model.LeaseKey{PlatformID: ttlInfo.PlatformID, Account: ttlInfo.Account}) == nil {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatal("expected ttl cleanup to delete lease")
}

func TestProxySessionManager_PortRangeAndLimit(t *testing.T) {
	env := newProxyE2EEnv(t)
	mgr := NewProxySessionManager(ProxySessionManagerConfig{
		Router:            env.router,
		Pool:              env.pool,
		Health:            &mockHealthRecorder{},
		Events:            newMockEventEmitter(),
		MinPort:           23000,
		MaxPort:           23000,
		MaxActiveSessions: 1,
		PortRetries:       4,
	})
	t.Cleanup(mgr.Close)

	info, err := mgr.Create(ProxySessionCreateRequest{PlatformName: "plat", TTL: time.Minute})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	if !strings.HasSuffix(info.ProxyURL, ":23000") {
		t.Fatalf("proxy_url port: got %q, want :23000", info.ProxyURL)
	}
	if _, err := mgr.Create(ProxySessionCreateRequest{PlatformName: "plat", TTL: time.Minute}); !errors.Is(err, ErrProxySessionLimitExceeded) {
		t.Fatalf("limit error: got %v, want ErrProxySessionLimitExceeded", err)
	}
}

func TestProxySessionManager_ProxyURLForwardsWithoutAuth(t *testing.T) {
	env := newProxyE2EEnv(t)
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if got := r.Header.Get("Proxy-Authorization"); got != "" {
			t.Fatalf("Proxy-Authorization leaked: %q", got)
		}
		_, _ = w.Write([]byte("session-ok"))
	}))
	defer upstream.Close()

	setProxyE2EOutboundDialFunc(t, env, func(ctx context.Context, network string, dest M.Socksaddr) (net.Conn, error) {
		return (&net.Dialer{}).DialContext(ctx, network, dest.String())
	})
	mgr := newTestProxySessionManager(t, env, 24000, 24100)
	info, err := mgr.Create(ProxySessionCreateRequest{PlatformName: "plat", TTL: time.Minute})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}

	proxyAddr := strings.TrimPrefix(info.ProxyURL, "http://")
	conn, err := net.Dial("tcp", proxyAddr)
	if err != nil {
		t.Fatalf("dial session proxy: %v", err)
	}
	defer conn.Close()
	if _, err := fmt.Fprintf(conn, "GET %s HTTP/1.1\r\nHost: %s\r\nConnection: close\r\n\r\n", upstream.URL, strings.TrimPrefix(upstream.URL, "http://")); err != nil {
		t.Fatalf("write request: %v", err)
	}
	resp, err := http.ReadResponse(bufio.NewReader(conn), nil)
	if err != nil {
		t.Fatalf("read response: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status: got %d, want %d", resp.StatusCode, http.StatusOK)
	}
}

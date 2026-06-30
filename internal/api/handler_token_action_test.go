package api

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"net/netip"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/Resinat/Resin/internal/model"
	"github.com/Resinat/Resin/internal/node"
	"github.com/Resinat/Resin/internal/proxy"
	"github.com/Resinat/Resin/internal/routing"
	"github.com/Resinat/Resin/internal/service"
	"github.com/Resinat/Resin/internal/subscription"
	"github.com/Resinat/Resin/internal/testutil"
)

func doTokenJSONRequest(t *testing.T, handler http.Handler, method, path string, body any) *httptest.ResponseRecorder {
	t.Helper()

	var reqBody []byte
	var err error
	if body != nil {
		reqBody, err = json.Marshal(body)
		if err != nil {
			t.Fatalf("marshal body: %v", err)
		}
	}

	req := httptest.NewRequest(method, path, bytes.NewReader(reqBody))
	if body != nil {
		req.Header.Set("Content-Type", "application/json; charset=utf-8")
	}
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)
	return rec
}

func TestTokenActionInheritLease_Success(t *testing.T) {
	srv, cp, _ := newControlPlaneTestServer(t)
	platformName := "token-lease-target"
	platformID := mustCreatePlatform(t, srv, platformName)

	nowNs := time.Now().UnixNano()
	parent := model.Lease{
		PlatformID:     platformID,
		Account:        "parent-account",
		NodeHash:       node.HashFromRawOptions([]byte(`{"id":"token-parent-node"}`)).Hex(),
		EgressIP:       "203.0.113.10",
		CreatedAtNs:    nowNs - int64(10*time.Minute),
		ExpiryNs:       nowNs + int64(30*time.Minute),
		LastAccessedNs: nowNs - int64(time.Minute),
	}
	if err := cp.Router.UpsertLease(parent); err != nil {
		t.Fatalf("seed parent lease: %v", err)
	}

	handler := NewTokenActionHandler("tok", cp, 1<<20)
	rec := doTokenJSONRequest(
		t,
		handler,
		http.MethodPost,
		"/tok/api/v1/"+platformName+"/actions/inherit-lease",
		map[string]any{
			"parent_account": "parent-account",
			"new_account":    "new-account",
		},
	)
	if rec.Code != http.StatusOK {
		t.Fatalf("status: got %d, want %d, body=%s", rec.Code, http.StatusOK, rec.Body.String())
	}

	body := decodeJSONMap(t, rec)
	if body["status"] != "ok" {
		t.Fatalf("status field: got %v, want %q", body["status"], "ok")
	}

	child := cp.Router.ReadLease(model.LeaseKey{PlatformID: platformID, Account: "new-account"})
	if child == nil {
		t.Fatal("expected new-account lease to be created")
	}
	if child.NodeHash != parent.NodeHash {
		t.Fatalf("child node_hash: got %q, want %q", child.NodeHash, parent.NodeHash)
	}
	if child.EgressIP != parent.EgressIP {
		t.Fatalf("child egress_ip: got %q, want %q", child.EgressIP, parent.EgressIP)
	}
	if child.ExpiryNs != parent.ExpiryNs {
		t.Fatalf("child expiry_ns: got %d, want %d", child.ExpiryNs, parent.ExpiryNs)
	}
}

func TestTokenActionInheritLease_RejectsUnknownFields(t *testing.T) {
	srv, cp, _ := newControlPlaneTestServer(t)
	platformName := "token-lease-unknown-field"
	_ = mustCreatePlatform(t, srv, platformName)

	handler := NewTokenActionHandler("tok", cp, 1<<20)
	rec := doTokenJSONRequest(
		t,
		handler,
		http.MethodPost,
		"/tok/api/v1/"+platformName+"/actions/inherit-lease",
		map[string]any{
			"parent_account": "parent",
			"new_account":    "child",
			"extra":          "unexpected",
		},
	)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("status: got %d, want %d, body=%s", rec.Code, http.StatusBadRequest, rec.Body.String())
	}
	assertErrorCode(t, rec, "INVALID_ARGUMENT")
}

func TestTokenActionInheritLease_ParentMissingOrExpiredReturnsNotFound(t *testing.T) {
	srv, cp, _ := newControlPlaneTestServer(t)
	platformName := "token-lease-parent-notfound"
	platformID := mustCreatePlatform(t, srv, platformName)
	handler := NewTokenActionHandler("tok", cp, 1<<20)

	rec := doTokenJSONRequest(
		t,
		handler,
		http.MethodPost,
		"/tok/api/v1/"+platformName+"/actions/inherit-lease",
		map[string]any{
			"parent_account": "missing-parent",
			"new_account":    "child",
		},
	)
	if rec.Code != http.StatusNotFound {
		t.Fatalf("missing parent status: got %d, want %d, body=%s", rec.Code, http.StatusNotFound, rec.Body.String())
	}
	assertErrorCode(t, rec, "NOT_FOUND")

	nowNs := time.Now().UnixNano()
	expired := model.Lease{
		PlatformID:     platformID,
		Account:        "expired-parent",
		NodeHash:       node.HashFromRawOptions([]byte(`{"id":"expired-token-parent-node"}`)).Hex(),
		EgressIP:       "203.0.113.22",
		CreatedAtNs:    nowNs - int64(2*time.Hour),
		ExpiryNs:       nowNs - int64(time.Second),
		LastAccessedNs: nowNs - int64(time.Minute),
	}
	if err := cp.Router.UpsertLease(expired); err != nil {
		t.Fatalf("seed expired lease: %v", err)
	}

	rec = doTokenJSONRequest(
		t,
		handler,
		http.MethodPost,
		"/tok/api/v1/"+platformName+"/actions/inherit-lease",
		map[string]any{
			"parent_account": "expired-parent",
			"new_account":    "child",
		},
	)
	if rec.Code != http.StatusNotFound {
		t.Fatalf("expired parent status: got %d, want %d, body=%s", rec.Code, http.StatusNotFound, rec.Body.String())
	}
	assertErrorCode(t, rec, "NOT_FOUND")
}

func TestTokenActionInheritLease_InvalidArguments(t *testing.T) {
	srv, cp, _ := newControlPlaneTestServer(t)
	platformName := "token-lease-invalid-args"
	_ = mustCreatePlatform(t, srv, platformName)
	handler := NewTokenActionHandler("tok", cp, 1<<20)

	rec := doTokenJSONRequest(
		t,
		handler,
		http.MethodPost,
		"/tok/api/v1/"+platformName+"/actions/inherit-lease",
		map[string]any{
			"parent_account": "same-account",
			"new_account":    "same-account",
		},
	)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("same account status: got %d, want %d, body=%s", rec.Code, http.StatusBadRequest, rec.Body.String())
	}
	assertErrorCode(t, rec, "INVALID_ARGUMENT")
}

func addTokenProxySessionNode(t *testing.T, cp *service.ControlPlaneService, raw string, ip string) node.Hash {
	t.Helper()
	hash := node.HashFromRawOptions([]byte(raw))
	sub := subscription.NewSubscription("proxy-session-sub", "proxy-session-sub", "https://example.com", true, false)
	cp.SubMgr.Register(sub)
	sub.ManagedNodes().StoreNode(hash, subscription.ManagedNode{Tags: []string{"tag"}})
	cp.Pool.AddNodeFromSub(hash, json.RawMessage(raw), sub.ID)
	entry, ok := cp.Pool.GetEntry(hash)
	if !ok {
		t.Fatal("node not found")
	}
	ob := testutil.NewNoopOutbound()
	entry.Outbound.Store(&ob)
	entry.SetEgressIP(netip.MustParseAddr(ip))
	entry.LatencyTable.Update("cloudflare.com", 20*time.Millisecond, 10*time.Minute)
	cp.Pool.RecordResult(hash, true)
	cp.Pool.NotifyNodeDirty(hash)
	return hash
}

func newTokenProxySessionHandler(t *testing.T, cp *service.ControlPlaneService, minPort, maxPort int, maxActive int) (*proxy.ProxySessionManager, http.Handler) {
	t.Helper()
	mgr := proxy.NewProxySessionManager(proxy.ProxySessionManagerConfig{
		Router:            cp.Router,
		Pool:              cp.Pool,
		Health:            cp.Pool,
		Events:            proxy.NoOpEventEmitter{},
		MinPort:           minPort,
		MaxPort:           maxPort,
		MaxActiveSessions: maxActive,
		PortRetries:       128,
	})
	t.Cleanup(mgr.Close)
	return mgr, NewTokenActionHandlerWithProxySessions("tok", cp, mgr, 1<<20)
}

func TestTokenProxySession_CreateJSONAndRelease(t *testing.T) {
	srv, cp, _ := newControlPlaneTestServer(t)
	platformName := "token-proxy-session"
	platformID := mustCreatePlatform(t, srv, platformName)
	first := addTokenProxySessionNode(t, cp, `{"type":"stub","server":"127.0.0.1","server_port":1}`, "203.0.113.10")

	_, handler := newTokenProxySessionHandler(t, cp, 25000, 25100, 1000)
	rec := doTokenJSONRequest(t, handler, http.MethodGet, "/tok/api/v1/"+platformName+"/proxy-sessions?ttl=30m", nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("create status: got %d, want %d, body=%s", rec.Code, http.StatusOK, rec.Body.String())
	}
	if got := rec.Header().Get("Cache-Control"); got != "no-store" {
		t.Fatalf("Cache-Control: got %q, want no-store", got)
	}
	body := decodeJSONMap(t, rec)
	id, _ := body["id"].(string)
	account, _ := body["account"].(string)
	proxyURL, _ := body["proxy_url"].(string)
	if id == "" || account == "" || proxyURL == "" {
		t.Fatalf("missing response fields: %s", rec.Body.String())
	}
	if body["node_hash"] != first.Hex() {
		t.Fatalf("node_hash: got %v, want %s", body["node_hash"], first.Hex())
	}
	if body["ttl"] != "30m0s" {
		t.Fatalf("ttl: got %v, want 30m0s", body["ttl"])
	}
	if !strings.HasPrefix(proxyURL, "http://127.0.0.1:") {
		t.Fatalf("proxy_url: got %q", proxyURL)
	}
	port := proxyURLPort(t, proxyURL)
	if port < 25000 || port > 25100 {
		t.Fatalf("port: got %d, want in range", port)
	}
	lease := cp.Router.ReadLease(model.LeaseKey{PlatformID: platformID, Account: account})
	if lease == nil {
		t.Fatal("expected lease after proxy session create")
	}

	rec = doTokenJSONRequest(t, handler, http.MethodDelete, "/tok/api/v1/"+platformName+"/proxy-sessions/"+id, nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("delete status: got %d, want %d, body=%s", rec.Code, http.StatusOK, rec.Body.String())
	}
	lease = cp.Router.ReadLease(model.LeaseKey{PlatformID: platformID, Account: account})
	if lease != nil {
		t.Fatal("expected lease deleted after release")
	}
	rec = doTokenJSONRequest(t, handler, http.MethodDelete, "/tok/api/v1/"+platformName+"/proxy-sessions/"+id, nil)
	if rec.Code != http.StatusNotFound {
		t.Fatalf("second delete status: got %d, want %d", rec.Code, http.StatusNotFound)
	}
	assertErrorCode(t, rec, "NOT_FOUND")
}

func TestTokenProxySession_FormatURLAndDefaultTTL(t *testing.T) {
	srv, cp, _ := newControlPlaneTestServer(t)
	platformName := "token-proxy-session-url"
	platformID := mustCreatePlatform(t, srv, platformName)
	addTokenProxySessionNode(t, cp, `{"type":"stub","server":"127.0.0.2","server_port":2}`, "203.0.113.20")
	_, handler := newTokenProxySessionHandler(t, cp, 25101, 25200, 1000)

	rec := doTokenJSONRequest(t, handler, http.MethodGet, "/tok/api/v1/"+platformName+"/proxy-sessions?format=url", nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("status: got %d, want %d, body=%s", rec.Code, http.StatusOK, rec.Body.String())
	}
	if got := rec.Header().Get("Content-Type"); !strings.HasPrefix(got, "text/plain") {
		t.Fatalf("content-type: got %q", got)
	}
	if !strings.HasPrefix(rec.Body.String(), "http://127.0.0.1:") {
		t.Fatalf("body: got %q", rec.Body.String())
	}
	var seen bool
	cp.Router.RangeLeases(platformID, func(_ string, lease routing.Lease) bool {
		seen = true
		wantTTL := 30 * time.Minute
		gotTTL := time.Duration(lease.ExpiryNs - lease.CreatedAtNs)
		if gotTTL < wantTTL-time.Second || gotTTL > wantTTL+time.Second {
			t.Fatalf("default ttl: got %v, want about %v", gotTTL, wantTTL)
		}
		return false
	})
	if !seen {
		t.Fatal("expected lease")
	}
}

func TestTokenProxySession_InvalidAndErrorCases(t *testing.T) {
	srv, cp, _ := newControlPlaneTestServer(t)
	platformName := "token-proxy-session-errors"
	_ = mustCreatePlatform(t, srv, platformName)
	addTokenProxySessionNode(t, cp, `{"type":"stub","server":"127.0.0.3","server_port":3}`, "203.0.113.30")
	_, handler := newTokenProxySessionHandler(t, cp, 25201, 25201, 1)

	for _, ttl := range []string{"bad", "0s", "-1s"} {
		rec := doTokenJSONRequest(t, handler, http.MethodGet, "/tok/api/v1/"+platformName+"/proxy-sessions?ttl="+ttl, nil)
		if rec.Code != http.StatusBadRequest {
			t.Fatalf("ttl %q status: got %d, want %d", ttl, rec.Code, http.StatusBadRequest)
		}
		assertErrorCode(t, rec, "INVALID_ARGUMENT")
	}

	rec := doTokenJSONRequest(t, handler, http.MethodGet, "/tok/api/v1/missing-platform/proxy-sessions", nil)
	if rec.Code != http.StatusNotFound {
		t.Fatalf("missing platform status: got %d, want %d", rec.Code, http.StatusNotFound)
	}
	assertErrorCode(t, rec, "NOT_FOUND")

	rec = doTokenJSONRequest(t, handler, http.MethodGet, "/tok/api/v1/"+platformName+"/proxy-sessions", nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("first create status: got %d, want %d", rec.Code, http.StatusOK)
	}
	rec = doTokenJSONRequest(t, handler, http.MethodGet, "/tok/api/v1/"+platformName+"/proxy-sessions", nil)
	if rec.Code != http.StatusConflict {
		t.Fatalf("limit status: got %d, want %d", rec.Code, http.StatusConflict)
	}
	assertErrorCode(t, rec, "CONFLICT")
}

func proxyURLPort(t *testing.T, raw string) int {
	t.Helper()
	parts := strings.Split(raw, ":")
	if len(parts) < 3 {
		t.Fatalf("invalid proxy url: %q", raw)
	}
	port, err := strconv.Atoi(parts[len(parts)-1])
	if err != nil {
		t.Fatalf("parse port: %v", err)
	}
	return port
}

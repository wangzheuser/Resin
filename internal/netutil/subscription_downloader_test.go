package netutil

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"net/netip"
	"net/url"
	"strconv"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/Resinat/Resin/internal/node"
)

func TestSubscriptionDownloadModesAndPreference(t *testing.T) {
	var directCalls, proxyCalls atomic.Int32
	direct := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		directCalls.Add(1)
		if r.UserAgent() != "clash.meta" {
			t.Error("wrong UA")
		}
		w.WriteHeader(503)
	}))
	defer direct.Close()
	proxy := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		proxyCalls.Add(1)
		_, _ = w.Write([]byte("via-external"))
	}))
	defer proxy.Close()
	d := &SubscriptionDownloader{ExternalProxyURL: proxy.URL}
	body, attempts, err := d.DownloadSubscription(context.Background(), direct.URL, "auto", "")
	if err != nil || string(body) != "via-external" || len(attempts) != 2 {
		t.Fatalf("fallback: %v %+v", err, attempts)
	}
	_, attempts, err = d.DownloadSubscription(context.Background(), direct.URL, "auto", "")
	if err != nil || len(attempts) != 1 || directCalls.Load() != 1 {
		t.Fatal("successful route preference not used")
	}
	d.Invalidate()
	_, _, _ = d.DownloadSubscription(context.Background(), direct.URL, "auto", "")
	if directCalls.Load() != 2 {
		t.Fatal("preference not invalidated")
	}
	before := proxyCalls.Load()
	_, attempts, err = d.DownloadSubscription(context.Background(), direct.URL, "direct", "")
	if err == nil || len(attempts) != 1 || proxyCalls.Load() != before {
		t.Fatal("direct mode changed route")
	}
	_, attempts, err = d.DownloadSubscription(context.Background(), direct.URL, "external_proxy", "")
	if err != nil || len(attempts) != 1 || attempts[0].Route != "external_proxy" {
		t.Fatal("explicit proxy failed")
	}
}

func TestSubscriptionDownloadDistinctInternalRoutesAndErrors(t *testing.T) {
	hashes := []node.Hash{node.HashFromRawOptions([]byte("first")), node.HashFromRawOptions([]byte("second"))}
	ips := []netip.Addr{netip.MustParseAddr("192.0.2.1"), netip.MustParseAddr("192.0.2.2")}
	picks := 0
	d := &SubscriptionDownloader{Pick: func(target, platform string, excluded map[node.Hash]struct{}, excludedIPs map[netip.Addr]struct{}) (DownloadRoute, error) {
		if target != "https://example.com/private?token=secret" || platform != "platform-id" {
			t.Error("route context lost")
		}
		if picks == 1 {
			if _, ok := excluded[hashes[0]]; !ok {
				t.Error("missing hash exclusion")
			}
			if _, ok := excludedIPs[ips[0]]; !ok {
				t.Error("missing IP exclusion")
			}
		}
		r := DownloadRoute{Hash: hashes[picks], EgressIP: ips[picks]}
		picks++
		return r, nil
	}, Fetch: func(ctx context.Context, h node.Hash, target string) ([]byte, error) {
		if h == hashes[0] {
			return nil, context.DeadlineExceeded
		}
		return nil, &HTTPStatusError{StatusCode: 404, URL: target}
	}}
	_, attempts, err := d.DownloadSubscription(context.Background(), "https://example.com/private?token=secret", "platform", "platform-id")
	if len(attempts) != 2 || attempts[0].Stage != "timeout" || attempts[1].HTTPStatus != 404 || ClassifySubscriptionError(err).Retryable {
		t.Fatalf("incorrect attempts: %+v, %v", attempts, err)
	}
	encoded, _ := json.Marshal(attempts)
	if strings.Contains(string(encoded), "secret") || strings.Contains(err.Error(), "private") {
		t.Fatal("credential leaked")
	}
}

func TestSubscriptionDownloadTimeoutAndCancellation(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { <-r.Context().Done() }))
	defer server.Close()
	d := &SubscriptionDownloader{Timeout: func() time.Duration { return 20 * time.Millisecond }}
	_, _, err := d.DownloadSubscription(context.Background(), server.URL, "direct", "")
	if ClassifySubscriptionError(err).Kind != "timeout" {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	_, attempts, err := d.DownloadSubscription(ctx, server.URL, "auto", "")
	if len(attempts) != 0 || ClassifySubscriptionError(err).Kind != "cancelled" {
		t.Fatal("cancelled request attempted network")
	}
	_, _, err = d.DownloadSubscription(context.Background(), server.URL, "external_proxy", "")
	if ClassifySubscriptionError(err).Kind != "configuration" {
		t.Fatal("missing proxy not diagnosed")
	}
}

func TestSubscriptionDownloadRetryAfterAndRedaction(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { w.Header().Set("Retry-After", "120"); w.WriteHeader(429) }))
	defer server.Close()
	d := &SubscriptionDownloader{ExternalProxyURL: "http://user:secret@127.0.0.1:1"}
	_, attempts, err := d.DownloadSubscription(context.Background(), server.URL, "auto", "")
	if len(attempts) != 1 || ClassifySubscriptionError(err).RetryAfter != 2*time.Minute {
		t.Fatal("ignored Retry-After")
	}
	if strings.Contains(ClassifySubscriptionError(errors.New("SOCKS password=secret")).Error(), "secret") {
		t.Fatal("secret leak")
	}
	for _, scheme := range []string{"http", "https", "socks5", "socks5h"} {
		if ValidateSubscriptionProxy(scheme+"://localhost:7890") != nil {
			t.Fatal(scheme)
		}
	}
	if ValidateSubscriptionProxy("ftp://localhost:7890") == nil {
		t.Fatal("invalid scheme accepted")
	}
}

func TestSubscriptionDownloadAutoRetriesInvalidSuccessfulBody(t *testing.T) {
	var directCalls, proxyCalls atomic.Int32
	direct := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		directCalls.Add(1)
		_, _ = w.Write([]byte("<html>blocked</html>"))
	}))
	defer direct.Close()
	proxy := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		proxyCalls.Add(1)
		_, _ = w.Write([]byte("valid"))
	}))
	defer proxy.Close()

	d := &SubscriptionDownloader{
		ExternalProxyURL: proxy.URL,
		ValidateBody: func(body []byte) error {
			if string(body) != "valid" {
				return errors.New("invalid subscription")
			}
			return nil
		},
	}
	body, attempts, err := d.DownloadSubscription(context.Background(), direct.URL, "auto", "")
	if err != nil || string(body) != "valid" || len(attempts) != 2 {
		t.Fatalf("expected invalid 200 fallback, body=%q attempts=%+v err=%v", body, attempts, err)
	}
	if directCalls.Load() != 1 || proxyCalls.Load() != 1 || attempts[0].Stage != "parse" {
		t.Fatalf("unexpected fallback trace: direct=%d proxy=%d attempts=%+v", directCalls.Load(), proxyCalls.Load(), attempts)
	}
}

func TestSubscriptionDownloadExternalSocks5AndSocks5h(t *testing.T) {
	target := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte("socks-valid"))
	}))
	defer target.Close()

	for _, scheme := range []string{"socks5", "socks5h"} {
		t.Run(scheme, func(t *testing.T) {
			proxyAddr, stop := startTestSocks5Proxy(t, "proxy-user", "proxy-pass")
			defer stop()
			d := &SubscriptionDownloader{ExternalProxyURL: fmt.Sprintf("%s://proxy-user:proxy-pass@%s", scheme, proxyAddr)}
			body, attempts, err := d.DownloadSubscription(context.Background(), target.URL, "external_proxy", "")
			if err != nil {
				t.Fatalf("download through %s: %v; attempts=%+v", scheme, err, attempts)
			}
			if string(body) != "socks-valid" || len(attempts) != 1 || attempts[0].Route != "external_proxy" {
				t.Fatalf("unexpected SOCKS result: body=%q attempts=%+v", body, attempts)
			}
		})
	}
}

func startTestSocks5Proxy(t *testing.T, username, password string) (string, func()) {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	stop := make(chan struct{})
	go func() {
		for {
			conn, err := ln.Accept()
			if err != nil {
				select {
				case <-stop:
					return
				default:
				}
				continue
			}
			go serveTestSocks5Conn(conn, username, password)
		}
	}()
	return ln.Addr().String(), func() {
		close(stop)
		_ = ln.Close()
	}
}

func serveTestSocks5Conn(conn net.Conn, username, password string) {
	defer conn.Close()
	r := bufio.NewReader(conn)
	version, err := r.ReadByte()
	if err != nil || version != 5 {
		return
	}
	n, err := r.ReadByte()
	if err != nil {
		return
	}
	methods := make([]byte, int(n))
	if _, err = io.ReadFull(r, methods); err != nil {
		return
	}
	if _, err = conn.Write([]byte{5, 2}); err != nil {
		return
	}
	if version, err = r.ReadByte(); err != nil || version != 1 {
		return
	}
	ulen, err := r.ReadByte()
	if err != nil {
		return
	}
	user := make([]byte, int(ulen))
	if _, err = io.ReadFull(r, user); err != nil {
		return
	}
	plen, err := r.ReadByte()
	if err != nil {
		return
	}
	pass := make([]byte, int(plen))
	if _, err = io.ReadFull(r, pass); err != nil || string(user) != username || string(pass) != password {
		_, _ = conn.Write([]byte{1, 1})
		return
	}
	if _, err = conn.Write([]byte{1, 0}); err != nil {
		return
	}
	if version, err = r.ReadByte(); err != nil || version != 5 {
		return
	}
	command, err := r.ReadByte()
	if err != nil || command != 1 {
		return
	}
	_, _ = r.ReadByte()
	addressType, err := r.ReadByte()
	if err != nil {
		return
	}
	host := ""
	switch addressType {
	case 1:
		ip := make([]byte, net.IPv4len)
		if _, err = io.ReadFull(r, ip); err != nil {
			return
		}
		host = net.IP(ip).String()
	case 3:
		length, readErr := r.ReadByte()
		if readErr != nil {
			return
		}
		name := make([]byte, int(length))
		if _, err = io.ReadFull(r, name); err != nil {
			return
		}
		host = string(name)
	case 4:
		ip := make([]byte, net.IPv6len)
		if _, err = io.ReadFull(r, ip); err != nil {
			return
		}
		host = net.IP(ip).String()
	default:
		return
	}
	portBytes := make([]byte, 2)
	if _, err = io.ReadFull(r, portBytes); err != nil {
		return
	}
	target, err := net.Dial("tcp", net.JoinHostPort(host, strconv.Itoa(int(portBytes[0])<<8|int(portBytes[1]))))
	if err != nil {
		_, _ = conn.Write([]byte{5, 5, 0, 1, 0, 0, 0, 0, 0, 0})
		return
	}
	defer target.Close()
	if _, err = conn.Write([]byte{5, 0, 0, 1, 0, 0, 0, 0, 0, 0}); err != nil {
		return
	}
	go func() { _, _ = io.Copy(target, r) }()
	_, _ = io.Copy(conn, target)
}

func TestSubscriptionProxyURLWithCredentialsIsRedacted(t *testing.T) {
	u, _ := url.Parse("socks5://user:secret@example.com:1080")
	if got := RedactURL(u.String()); strings.Contains(got, "secret") || strings.Contains(got, "user") {
		t.Fatalf("redacted proxy URL leaked credentials: %q", got)
	}
}

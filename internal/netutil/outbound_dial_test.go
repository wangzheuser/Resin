package netutil

import (
	"context"
	"errors"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
	"time"

	"github.com/sagernet/sing-box/adapter"
	M "github.com/sagernet/sing/common/metadata"
)

type lazyHTTPOutbound struct {
	adapter.Outbound
	dial func(context.Context, string, M.Socksaddr) (net.Conn, error)
}

func (o *lazyHTTPOutbound) DialContext(ctx context.Context, network string, dst M.Socksaddr) (net.Conn, error) {
	return o.dial(ctx, network, dst)
}

// Like WebSocket early data, the first write still needs the dial context.
type lazyHTTPConn struct {
	net.Conn
	ctx       context.Context
	completed bool
	stall     bool
	closed    atomic.Bool
}

func (c *lazyHTTPConn) NeedHandshake() bool { return !c.completed }
func (c *lazyHTTPConn) Write(p []byte) (int, error) {
	if !c.completed {
		if err := c.ctx.Err(); err != nil {
			return 0, err
		}
		if c.stall {
			return c.Conn.Write([]byte("handshake"))
		}
		c.completed = true
	}
	if len(p) == 0 {
		return 0, nil
	}
	return c.Conn.Write(p)
}

func (c *lazyHTTPConn) Close() error {
	c.closed.Store(true)
	return c.Conn.Close()
}

func TestHTTPGetViaOutboundEarlyHandshake(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		// A setup deadline must not become an established stream lifetime.
		time.Sleep(120 * time.Millisecond)
		_, _ = io.WriteString(w, "early-data-ok")
	}))
	defer srv.Close()
	var conn *lazyHTTPConn
	ob := &lazyHTTPOutbound{dial: func(ctx context.Context, network string, dst M.Socksaddr) (net.Conn, error) {
		var d net.Dialer
		c, err := d.DialContext(ctx, network, dst.String())
		if err != nil {
			return nil, err
		}
		conn = &lazyHTTPConn{Conn: c, ctx: ctx}
		return conn, nil
	}}
	ctx := WithDialDeadline(context.Background(), time.Now().Add(80*time.Millisecond))
	body, _, err := HTTPGetViaOutbound(ctx, ob, srv.URL, OutboundHTTPOptions{})
	if err != nil || string(body) != "early-data-ok" {
		t.Fatalf("lazy handshake request: body=%q err=%v", body, err)
	}
	if !conn.completed || !conn.closed.Load() {
		t.Fatal("handshake did not finish or response connection was not released")
	}
}

func TestHTTPGetViaOutboundEarlyHandshakeTimeoutCloses(t *testing.T) {
	client, peer := net.Pipe()
	defer peer.Close()
	defer client.Close()
	_ = client.SetWriteDeadline(time.Now().Add(time.Second))
	var conn *lazyHTTPConn
	ob := &lazyHTTPOutbound{dial: func(ctx context.Context, _ string, _ M.Socksaddr) (net.Conn, error) {
		conn = &lazyHTTPConn{Conn: client, ctx: ctx, stall: true}
		return conn, nil
	}}
	ctx := WithDialDeadline(context.Background(), time.Now().Add(30*time.Millisecond))
	start := time.Now()
	_, _, err := HTTPGetViaOutbound(ctx, ob, "http://example.test/", OutboundHTTPOptions{})
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("handshake error=%v, want deadline exceeded", err)
	}
	if time.Since(start) > 400*time.Millisecond || !conn.closed.Load() {
		t.Fatal("stalled handshake did not release its connection within the budget")
	}
}

package netutil

import (
	"context"
	"crypto/tls"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptrace"
	"sync"
	"time"

	"github.com/sagernet/sing-box/adapter"
	M "github.com/sagernet/sing/common/metadata"
)

const defaultOutboundUserAgent = "Resin/1.0"

type ConnLifecycleOp uint8

const (
	ConnLifecycleOpen ConnLifecycleOp = iota
	ConnLifecycleClose
)

type dialDeadlineContextKey struct{}

// WithDialDeadline preserves an HTTP request deadline for custom dialers.
// net/http intentionally detaches its dial context from request cancellation.
func WithDialDeadline(ctx context.Context, deadline time.Time) context.Context {
	if ctx == nil || deadline.IsZero() {
		return ctx
	}
	return context.WithValue(ctx, dialDeadlineContextKey{}, deadline)
}

// ApplyDialDeadline restores a request deadline on a transport dial context.
func ApplyDialDeadline(ctx context.Context) (context.Context, context.CancelFunc) {
	if ctx == nil {
		return context.Background(), func() {}
	}
	deadline, ok := ctx.Value(dialDeadlineContextKey{}).(time.Time)
	if !ok || deadline.IsZero() {
		return ctx, func() {}
	}
	if current, ok := ctx.Deadline(); ok && !deadline.Before(current) {
		return ctx, func() {}
	}
	return context.WithDeadline(ctx, deadline)
}

// OutboundHTTPOptions controls outbound-backed HTTP execution behavior.
type OutboundHTTPOptions struct {
	// RequireStatusOK enforces HTTP 200 status; otherwise any status is accepted.
	RequireStatusOK bool
	// UserAgent overrides the request User-Agent when non-empty.
	UserAgent string
	// OnConnLifecycle is called with open/close lifecycle events to track connection
	// lifecycle for metrics. Set by probe callers to count outbound connections;
	// left nil for download callers (GeoIP, subscription) to exclude from stats.
	OnConnLifecycle func(op ConnLifecycleOp)
}

// HTTPGetViaTransport executes an HTTP GET with a caller-owned transport.
// The transport may be reused across requests; callers own its lifecycle and
// should close idle connections when the owning pool is stopped or evicted.
func HTTPGetViaTransport(
	ctx context.Context,
	transport *http.Transport,
	url string,
	opts OutboundHTTPOptions,
) ([]byte, time.Duration, error) {
	if transport == nil {
		return nil, 0, fmt.Errorf("outbound fetch: transport is nil")
	}
	return httpGetWithClient(ctx, &http.Client{Transport: transport}, url, opts)
}

// HTTPGetViaOutbound executes an HTTP GET through the provided outbound.
// Timeout and cancellation are controlled solely by ctx.
func HTTPGetViaOutbound(
	ctx context.Context,
	outbound adapter.Outbound,
	url string,
	opts OutboundHTTPOptions,
) ([]byte, time.Duration, error) {
	if outbound == nil {
		return nil, 0, fmt.Errorf("outbound fetch: outbound is nil")
	}

	transport := &http.Transport{
		DialContext: func(ctx context.Context, network, addr string) (net.Conn, error) {
			dialCtx, cancel := ApplyDialDeadline(ctx)
			defer cancel()
			conn, err := outbound.DialContext(dialCtx, network, M.ParseSocksaddr(addr))
			if err != nil {
				return nil, err
			}
			if opts.OnConnLifecycle != nil {
				opts.OnConnLifecycle(ConnLifecycleOpen)
				return &connCloseHook{Conn: conn, onClose: func() { opts.OnConnLifecycle(ConnLifecycleClose) }}, nil
			}
			return conn, nil
		},
		DisableKeepAlives: true,
		ForceAttemptHTTP2: true,
	}
	defer transport.CloseIdleConnections()

	client := &http.Client{Transport: transport}
	return httpGetWithClient(ctx, client, url, opts)
}

func httpGetWithClient(
	ctx context.Context,
	client *http.Client,
	url string,
	opts OutboundHTTPOptions,
) ([]byte, time.Duration, error) {
	if client == nil || client.Transport == nil {
		return nil, 0, fmt.Errorf("outbound fetch: client transport is nil")
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return nil, 0, err
	}

	userAgent := opts.UserAgent
	if userAgent == "" {
		userAgent = defaultOutboundUserAgent
	}
	req.Header.Set("User-Agent", userAgent)

	requestStart := time.Now()
	var start, firstByte time.Time
	var latency time.Duration
	trace := &httptrace.ClientTrace{
		TLSHandshakeStart: func() { start = time.Now() },
		TLSHandshakeDone: func(_ tls.ConnectionState, err error) {
			if err == nil {
				latency = time.Since(start)
			}
		},
		GotFirstResponseByte: func() { firstByte = time.Now() },
	}
	requestCtx := httptrace.WithClientTrace(ctx, trace)
	if deadline, ok := ctx.Deadline(); ok {
		requestCtx = WithDialDeadline(requestCtx, deadline)
	}
	req = req.WithContext(requestCtx)

	resp, err := client.Do(req)
	if err != nil {
		return nil, 0, err
	}
	defer resp.Body.Close()

	requestDone := time.Now()
	if latency <= 0 {
		switch {
		case !firstByte.IsZero() && firstByte.After(requestStart):
			latency = firstByte.Sub(requestStart)
		default:
			latency = requestDone.Sub(requestStart)
		}
		if latency <= 0 {
			latency = time.Nanosecond
		}
	}
	if opts.RequireStatusOK && resp.StatusCode != http.StatusOK {
		return nil, latency, fmt.Errorf("outbound fetch: unexpected status %d from %s", resp.StatusCode, url)
	}

	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, latency, err
	}

	return body, latency, nil
}

// connCloseHook wraps a net.Conn and calls onClose exactly once on Close.
type connCloseHook struct {
	net.Conn
	onClose   func()
	closeOnce sync.Once
	closeErr  error
}

func (c *connCloseHook) Close() error {
	c.closeOnce.Do(func() {
		if c.onClose != nil {
			c.onClose()
		}
		c.closeErr = c.Conn.Close()
	})
	return c.closeErr
}

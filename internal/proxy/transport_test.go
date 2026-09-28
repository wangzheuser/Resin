package proxy

import (
	"context"
	"errors"
	"net"
	"sync/atomic"
	"testing"
	"time"

	"github.com/Resinat/Resin/internal/netutil"
	"github.com/Resinat/Resin/internal/node"
	"github.com/sagernet/sing-box/adapter"
	M "github.com/sagernet/sing/common/metadata"
)

type transportMetricsSink struct {
	open  atomic.Int32
	close atomic.Int32
}

func (s *transportMetricsSink) OnTrafficDelta(int64, int64) {}
func (s *transportMetricsSink) OnConnectionLifecycle(_ ConnectionDirection, op ConnectionOp) {
	switch op {
	case ConnectionOpen:
		s.open.Add(1)
	case ConnectionClose:
		s.close.Add(1)
	}
}

type noopOutbound struct {
	adapter.Outbound
}

func (n *noopOutbound) DialContext(context.Context, string, M.Socksaddr) (net.Conn, error) {
	return nil, errors.New("not used in transport-pool tests")
}

func (n *noopOutbound) Tag() string  { return "noop" }
func (n *noopOutbound) Type() string { return "noop" }

func TestOutboundTransportPool_ReusesByNodeHash(t *testing.T) {
	pool := newOutboundTransportPool()
	hash := node.Hash{1}

	t1 := pool.Get(hash, &noopOutbound{}, nil)
	t2 := pool.Get(hash, &noopOutbound{}, nil)

	if t1 != t2 {
		t.Fatal("expected same transport instance for identical node hash")
	}
}

func TestOutboundTransportPool_SetsDefaultMetricsSink(t *testing.T) {
	p := newOutboundTransportPool()
	sink := &transportMetricsSink{}
	p.SetMetricsSink(sink)
	p.mu.Lock()
	got := p.defaultSink
	p.mu.Unlock()
	if got != sink {
		t.Fatal("default metrics sink was not retained")
	}
}

func TestOutboundTransportPool_SplitsByNodeHash(t *testing.T) {
	pool := newOutboundTransportPool()
	ob := &noopOutbound{}
	hash1 := node.Hash{1}
	hash2 := node.Hash{2}

	base := pool.Get(hash1, ob, nil)
	byNodeHash := pool.Get(hash2, ob, nil)
	if base == byNodeHash {
		t.Fatal("expected different transport for different node hash")
	}
}

func TestOutboundTransportPool_UsesKeepAliveTransport(t *testing.T) {
	pool := newOutboundTransportPool()
	ob := &noopOutbound{}
	hash := node.Hash{1}

	transport := pool.Get(hash, ob, nil)
	if transport.DisableKeepAlives {
		t.Fatal("expected keep-alive enabled transport")
	}
}

func TestOutboundTransportPool_EvictRemovesNodeTransport(t *testing.T) {
	pool := newOutboundTransportPool()
	hash := node.Hash{1}
	ob := &noopOutbound{}

	t1 := pool.Get(hash, ob, nil)
	pool.Evict(hash)
	t2 := pool.Get(hash, ob, nil)

	if t1 == t2 {
		t.Fatal("expected a new transport after evict")
	}
}

func TestOutboundTransportPool_AppliesConfiguredLimits(t *testing.T) {
	pool := newOutboundTransportPoolWithConfig(OutboundTransportConfig{
		MaxIdleConns:        9,
		MaxIdleConnsPerHost: 3,
		IdleConnTimeout:     12 * time.Second,
		MaxTransports:       7,
	})
	ob := &noopOutbound{}
	hash := node.Hash{1}

	transport := pool.Get(hash, ob, nil)
	if transport.MaxIdleConns != 9 {
		t.Fatalf("MaxIdleConns: got %d, want %d", transport.MaxIdleConns, 9)
	}
	if transport.MaxIdleConnsPerHost != 3 {
		t.Fatalf("MaxIdleConnsPerHost: got %d, want %d", transport.MaxIdleConnsPerHost, 3)
	}
	if transport.IdleConnTimeout != 12*time.Second {
		t.Fatalf("IdleConnTimeout: got %s, want %s", transport.IdleConnTimeout, 12*time.Second)
	}
}

func TestOutboundTransportPool_BoundsRetainedNodeTransports(t *testing.T) {
	pool := newOutboundTransportPoolWithConfig(OutboundTransportConfig{MaxTransports: 2})
	ob := &noopOutbound{}
	first := pool.Get(node.Hash{1}, ob, nil)
	_ = pool.Get(node.Hash{2}, ob, nil)
	third := pool.Get(node.Hash{3}, ob, nil)

	if got := pool.transportCount(); got != 2 {
		t.Fatalf("retained transport count = %d, want 2", got)
	}
	if again := pool.Get(node.Hash{1}, ob, nil); again == first {
		t.Fatal("expected oldest node transport to be evicted")
	}
	if pool.Get(node.Hash{3}, ob, nil) != third {
		t.Fatal("newest node transport should remain pooled")
	}
}

func TestOutboundTransportPool_CloseAllClearsEntries(t *testing.T) {
	pool := newOutboundTransportPool()
	ob := &noopOutbound{}

	hashA := node.Hash{1}
	hashB := node.Hash{2}
	t1 := pool.Get(hashA, ob, nil)
	_ = pool.Get(hashB, ob, nil)

	pool.CloseAll()

	t2 := pool.Get(hashA, ob, nil)
	if t1 == t2 {
		t.Fatal("expected a new transport after CloseAll")
	}
}

func TestOutboundTransportPool_DialHasDeadline(t *testing.T) {
	pool := newOutboundTransportPool()
	defer pool.CloseAll()
	hasDeadline := false
	ob := &mockOutbound{dialFunc: func(ctx context.Context, _ string, _ M.Socksaddr) (net.Conn, error) {
		_, hasDeadline = ctx.Deadline()
		return nil, errors.New("test dial finished")
	}}
	transport := pool.Get(node.Hash{1}, ob, nil)
	_, _ = transport.DialContext(netutil.WithDialDeadline(context.Background(), time.Now().Add(time.Second)), "tcp", "example.test:443")
	if !hasDeadline {
		t.Fatal("outbound dial has no deadline after HTTP transport detaches request cancellation")
	}
	if transport.TLSHandshakeTimeout <= 0 {
		t.Fatal("TLS handshake has no timeout after HTTP transport detaches request cancellation")
	}
}

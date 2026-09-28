package proxy

import (
	"context"
	"net"
	"net/http"
	"sync"
	"time"

	"github.com/Resinat/Resin/internal/netutil"
	"github.com/Resinat/Resin/internal/node"
	"github.com/puzpuzpuz/xsync/v4"
	"github.com/sagernet/sing-box/adapter"
	M "github.com/sagernet/sing/common/metadata"
)

type OutboundTransportConfig struct {
	MaxIdleConns        int
	MaxIdleConnsPerHost int
	IdleConnTimeout     time.Duration
	// MaxTransports bounds the number of per-node HTTP transports retained in
	// the shared pool. Per-transport idle limits are not process-wide caps.
	MaxTransports int
}

const (
	defaultTransportMaxIdleConns        = 1024
	defaultTransportMaxIdleConnsPerHost = 64
	defaultTransportIdleConnTimeout     = 90 * time.Second
	defaultTransportMaxTransports       = 256
	defaultTransportTLSHandshakeTimeout = 10 * time.Second
)

func normalizeOutboundTransportConfig(cfg OutboundTransportConfig) OutboundTransportConfig {
	if cfg.MaxIdleConns <= 0 {
		cfg.MaxIdleConns = defaultTransportMaxIdleConns
	}
	if cfg.MaxIdleConnsPerHost <= 0 {
		cfg.MaxIdleConnsPerHost = defaultTransportMaxIdleConnsPerHost
	}
	if cfg.IdleConnTimeout <= 0 {
		cfg.IdleConnTimeout = defaultTransportIdleConnTimeout
	}
	if cfg.MaxTransports <= 0 {
		cfg.MaxTransports = defaultTransportMaxTransports
	}
	return cfg
}

// OutboundTransportPool manages reusable outbound HTTP transports keyed by node hash.
// A single instance should be shared by forward/reverse proxies so keep-alive pools
// are reused and can be evicted on node removal.
type OutboundTransportPool struct {
	config      OutboundTransportConfig
	transports  *xsync.Map[node.Hash, *http.Transport]
	mu          sync.Mutex
	order       []transportEntry
	defaultSink MetricsEventSink
}

type transportEntry struct {
	hash      node.Hash
	transport *http.Transport
}

func newOutboundTransportPool() *OutboundTransportPool {
	return NewOutboundTransportPool(OutboundTransportConfig{})
}

func newOutboundTransportPoolWithConfig(cfg OutboundTransportConfig) *OutboundTransportPool {
	return NewOutboundTransportPool(cfg)
}

// NewOutboundTransportPool creates a transport pool with normalized settings.
func NewOutboundTransportPool(cfg OutboundTransportConfig) *OutboundTransportPool {
	return &OutboundTransportPool{
		config:     normalizeOutboundTransportConfig(cfg),
		transports: xsync.NewMap[node.Hash, *http.Transport](),
	}
}

// SetMetricsSink sets the sink used when callers do not provide one explicitly.
// It is applied to transports created after the call; existing transports keep
// their sink so connection accounting remains consistent for their lifetime.
func (p *OutboundTransportPool) SetMetricsSink(sink MetricsEventSink) {
	if p == nil {
		return
	}
	p.mu.Lock()
	p.defaultSink = sink
	p.mu.Unlock()
}

// Get returns a reusable transport for the given node hash.
func (p *OutboundTransportPool) Get(
	hash node.Hash,
	ob adapter.Outbound,
	sink MetricsEventSink,
) *http.Transport {
	p.mu.Lock()
	if transport, ok := p.transports.Load(hash); ok {
		p.mu.Unlock()
		return transport
	}
	if sink == nil {
		sink = p.defaultSink
	}
	transport := p.newReusableOutboundTransport(ob, sink)
	p.transports.Store(hash, transport)
	p.order = append(p.order, transportEntry{hash: hash, transport: transport})
	var evicted []*http.Transport
	for len(p.order) > p.config.MaxTransports {
		oldest := p.order[0]
		p.order = p.order[1:]
		current, ok := p.transports.Load(oldest.hash)
		if !ok || current != oldest.transport {
			continue
		}
		p.transports.Delete(oldest.hash)
		evicted = append(evicted, oldest.transport)
	}
	p.mu.Unlock()
	for _, old := range evicted {
		old.CloseIdleConnections()
	}
	return transport
}

// Evict closes idle connections for one node transport and removes it from pool.
func (p *OutboundTransportPool) Evict(hash node.Hash) {
	p.mu.Lock()
	transport, ok := p.transports.LoadAndDelete(hash)
	p.mu.Unlock()
	if !ok || transport == nil {
		return
	}
	transport.CloseIdleConnections()
}

// CloseAll closes idle connections and clears all pooled transports.
func (p *OutboundTransportPool) CloseAll() {
	p.mu.Lock()
	var transports []*http.Transport
	p.transports.Range(func(_ node.Hash, transport *http.Transport) bool {
		if transport != nil {
			transports = append(transports, transport)
		}
		return true
	})
	p.transports.Clear()
	p.order = nil
	p.mu.Unlock()
	for _, transport := range transports {
		transport.CloseIdleConnections()
	}
}

func (p *OutboundTransportPool) transportCount() int {
	p.mu.Lock()
	defer p.mu.Unlock()
	count := 0
	p.transports.Range(func(_ node.Hash, _ *http.Transport) bool {
		count++
		return true
	})
	return count
}

func (p *OutboundTransportPool) newReusableOutboundTransport(ob adapter.Outbound, sink MetricsEventSink) *http.Transport {
	return &http.Transport{
		DialContext: func(ctx context.Context, network, addr string) (net.Conn, error) {
			dialCtx, cancel := netutil.ApplyDialDeadline(ctx)
			defer cancel()
			conn, err := ob.DialContext(dialCtx, network, M.ParseSocksaddr(addr))
			if err != nil {
				return nil, err
			}
			if sink != nil {
				sink.OnConnectionLifecycle(ConnectionOutbound, ConnectionOpen)
				conn = newCountingConn(conn, sink)
			}
			return conn, nil
		},
		DisableKeepAlives:   false,
		ForceAttemptHTTP2:   true,
		TLSHandshakeTimeout: defaultTransportTLSHandshakeTimeout,
		MaxIdleConns:        p.config.MaxIdleConns,
		MaxIdleConnsPerHost: p.config.MaxIdleConnsPerHost,
		IdleConnTimeout:     p.config.IdleConnTimeout,
	}
}

func newDirectHTTPTransport(cfg OutboundTransportConfig, sink MetricsEventSink) *http.Transport {
	cfg = normalizeOutboundTransportConfig(cfg)
	dialer := &net.Dialer{}
	return &http.Transport{
		DialContext: func(ctx context.Context, network, addr string) (net.Conn, error) {
			dialCtx, cancel := netutil.ApplyDialDeadline(ctx)
			defer cancel()
			conn, err := dialer.DialContext(dialCtx, network, addr)
			if err != nil {
				return nil, err
			}
			if sink != nil {
				sink.OnConnectionLifecycle(ConnectionOutbound, ConnectionOpen)
				conn = newCountingConn(conn, sink)
			}
			return conn, nil
		},
		DisableKeepAlives:   false,
		ForceAttemptHTTP2:   true,
		TLSHandshakeTimeout: defaultTransportTLSHandshakeTimeout,
		MaxIdleConns:        cfg.MaxIdleConns,
		MaxIdleConnsPerHost: cfg.MaxIdleConnsPerHost,
		IdleConnTimeout:     cfg.IdleConnTimeout,
	}
}

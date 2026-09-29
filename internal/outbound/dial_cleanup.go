package outbound

import (
	"context"
	"net"
	"sync"

	"github.com/Resinat/Resin/internal/netutil"
	"github.com/sagernet/sing-box/adapter"
	sbOutbound "github.com/sagernet/sing-box/adapter/outbound"
	"github.com/sagernet/sing-box/common/dialer"
	C "github.com/sagernet/sing-box/constant"
	"github.com/sagernet/sing-box/option"
	"github.com/sagernet/sing-box/transport/v2raywebsocket"
	"github.com/sagernet/sing/common"
	M "github.com/sagernet/sing/common/metadata"
	N "github.com/sagernet/sing/common/network"
	"github.com/sagernet/sing/service"
)

const ownedDialerTag = "resin-owned-server-dialer"

// sing-box 1.12.21 loses the raw socket on several TLS/WebSocket handshake
// errors, and its HTTP/SOCKS handshakes do not observe context cancellation.
// Track sockets below the protocol handshake and release them when the
// complete dial fails or is canceled. Successful connections and protocol
// wrappers stay untouched. Shared multiplex/HTTP2 transports own their sockets.
func withDialCleanup(ctx context.Context, opts any) (context.Context, bool, error) {
	var server option.ServerOptions
	var transport *option.V2RayTransportOptions
	var multiplex *option.OutboundMultiplexOptions
	switch o := opts.(type) {
	case *option.VLESSOutboundOptions:
		server, transport, multiplex = o.ServerOptions, o.Transport, o.Multiplex
	case *option.VMessOutboundOptions:
		server, transport, multiplex = o.ServerOptions, o.Transport, o.Multiplex
	case *option.TrojanOutboundOptions:
		server, transport, multiplex = o.ServerOptions, o.Transport, o.Multiplex
	case *option.HTTPOutboundOptions:
		server = o.ServerOptions
	case *option.SOCKSOutboundOptions:
		server = o.ServerOptions
	default:
		return ctx, false, nil
	}
	if multiplex != nil && multiplex.Enabled {
		return ctx, false, nil
	}
	if transport != nil {
		switch transport.Type {
		case C.V2RayTransportTypeWebsocket, C.V2RayTransportTypeHTTPUpgrade:
		default:
			return ctx, false, nil
		}
	}
	options := opts.(option.DialerOptionsWrapper)
	rawDialer, err := dialer.New(ctx, options.TakeDialerOptions(), server.ServerIsDomain())
	if err != nil {
		return ctx, false, err
	}
	owned := &ownedServerDialer{
		Adapter: sbOutbound.NewAdapter(ownedDialerTag, ownedDialerTag, []string{N.NetworkTCP, N.NetworkUDP}, nil),
		Dialer:  rawDialer,
	}
	manager := &singleOutboundManager{OutboundManager: service.FromContext[adapter.OutboundManager](ctx), owned: owned}
	// A per-build registry avoids overwriting another node's manager during
	// parallel warmup and does not add per-node entries to the global manager.
	registry := &dialServiceRegistry{Registry: service.NewRegistry(), parent: service.RegistryFromContext(ctx)}
	ctx = service.ContextWithRegistry(ctx, registry)
	service.MustRegister[adapter.OutboundManager](ctx, manager)
	options.ReplaceDialerOptions(option.DialerOptions{Detour: ownedDialerTag})
	return ctx, true, nil
}

type dialServiceRegistry struct {
	service.Registry
	parent service.Registry
}

func (r *dialServiceRegistry) Get(key any) any {
	if value := r.Registry.Get(key); value != nil {
		return value
	}
	return r.parent.Get(key)
}

type singleOutboundManager struct {
	adapter.OutboundManager
	owned adapter.Outbound
}

func (m *singleOutboundManager) Outbound(tag string) (adapter.Outbound, bool) {
	if tag == ownedDialerTag {
		return m.owned, true
	}
	return m.OutboundManager.Outbound(tag)
}

type dialAttemptKey struct{}

type dialAttempt struct {
	mu     sync.Mutex
	conns  []net.Conn
	done   bool
	failed bool
}

func (a *dialAttempt) track(conn net.Conn) {
	a.mu.Lock()
	if !a.done {
		a.conns = append(a.conns, conn)
	}
	closeConn := a.done && a.failed
	a.mu.Unlock()
	if closeConn {
		_ = conn.Close()
	}
}

func (a *dialAttempt) finish(failed bool) {
	a.mu.Lock()
	if a.done {
		a.mu.Unlock()
		return
	}
	a.done, a.failed = true, failed
	conns := a.conns
	a.conns = nil
	a.mu.Unlock()
	if failed {
		for _, conn := range conns {
			_ = conn.Close()
		}
	}
}

type ownedServerDialer struct {
	sbOutbound.Adapter
	N.Dialer
}

func (d *ownedServerDialer) DialContext(ctx context.Context, network string, destination M.Socksaddr) (net.Conn, error) {
	conn, err := d.Dialer.DialContext(ctx, network, destination)
	if conn != nil {
		if attempt, ok := ctx.Value(dialAttemptKey{}).(*dialAttempt); ok {
			attempt.track(conn)
		}
	}
	return conn, err
}

type cleanupOutbound struct {
	adapter.Outbound
	dependencies []string
}

func (o *cleanupOutbound) Dependencies() []string { return o.dependencies }
func (o *cleanupOutbound) Close() error           { return common.Close(o.Outbound) }

func (o *cleanupOutbound) DialContext(ctx context.Context, network string, destination M.Socksaddr) (net.Conn, error) {
	attempt := &dialAttempt{}
	ctx = context.WithValue(ctx, dialAttemptKey{}, attempt)
	stop := context.AfterFunc(ctx, func() { attempt.finish(true) })
	defer stop()
	defer attempt.finish(true)
	conn, err := o.Outbound.DialContext(ctx, network, destination)
	if err == nil {
		if _, early := common.Cast[*v2raywebsocket.EarlyWebsocketConn](conn); early {
			// Keep tracking sockets until the deferred WebSocket handshake ends.
			// Other protocols retain their existing lazy request writes.
			err = netutil.CompleteEarlyHandshake(ctx, conn)
		}
	}
	if !stop() {
		_ = common.Close(conn)
		return nil, ctx.Err()
	}
	attempt.finish(err != nil)
	return conn, err
}

func (o *cleanupOutbound) ListenPacket(ctx context.Context, destination M.Socksaddr) (net.PacketConn, error) {
	attempt := &dialAttempt{}
	ctx = context.WithValue(ctx, dialAttemptKey{}, attempt)
	stop := context.AfterFunc(ctx, func() { attempt.finish(true) })
	defer stop()
	defer attempt.finish(true)
	conn, err := o.Outbound.ListenPacket(ctx, destination)
	if !stop() {
		_ = common.Close(conn)
		return nil, ctx.Err()
	}
	attempt.finish(err != nil)
	return conn, err
}

package proxy

import (
	"bufio"
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"io"
	"log"
	"net"
	"net/netip"
	"sync"
	"time"

	"github.com/Resinat/Resin/internal/netutil"
	"github.com/Resinat/Resin/internal/outbound"
	"github.com/Resinat/Resin/internal/routing"
	"github.com/sagernet/sing/common"
	M "github.com/sagernet/sing/common/metadata"
	N "github.com/sagernet/sing/common/network"
)

type tunnelDeps struct {
	router      *routing.Router
	pool        outbound.PoolAccessor
	health      HealthRecorder
	metricsSink MetricsEventSink
	bypass      *TargetBypassMatcher
}

type preparedTunnel struct {
	upstreamConn net.Conn
	recordResult func(bool, string, bool)
}

type tunnelPrepareResult struct {
	route         routing.RouteResult
	session       *preparedTunnel
	proxyErr      *ProxyError
	upstreamStage string
	upstreamErr   error
	canceled      bool
}

type tunnelRelayResult struct {
	ingressBytes  int64
	egressBytes   int64
	netOK         bool
	proxyErr      *ProxyError
	upstreamStage string
	upstreamErr   error
}

type tunnelPumpOptions struct {
	requireBidirectionalTraffic bool
}

func prepareConnectTunnel(
	ctx context.Context,
	deps tunnelDeps,
	platformName string,
	account string,
	target string,
) tunnelPrepareResult {
	if deps.bypass != nil && deps.bypass.ShouldBypass(target) {
		return prepareDirectConnectTunnel(ctx, deps, target)
	}

	excludedEgressIPs := make(map[netip.Addr]struct{}, 1)
	var initialRoute routing.RouteResult
	var initialProxyErr *ProxyError
	var initialUpstreamErr error
	var routed routedOutbound
	for attempt := 0; attempt < 2; attempt++ {
		var routeErr *ProxyError
		if attempt == 0 {
			routed, routeErr = resolveRoutedOutbound(deps.router, deps.pool, platformName, account, target)
		} else {
			routed, routeErr = resolveRoutedOutboundExcluding(
				deps.router,
				deps.pool,
				platformName,
				account,
				target,
				excludedEgressIPs,
			)
		}
		if routeErr != nil {
			if attempt > 0 {
				failed := false
				deps.router.RecordRouteFailover(&failed)
				logRouteFailover("failed", initialRoute, routing.RouteResult{}, target, "route_unavailable")
				return tunnelPrepareResult{
					route:         initialRoute,
					proxyErr:      initialProxyErr,
					upstreamStage: "connect_dial",
					upstreamErr:   initialUpstreamErr,
				}
			}
			return tunnelPrepareResult{route: initialRoute, proxyErr: routeErr}
		}
		if attempt == 0 {
			initialRoute = routed.Route
		}

		domain := netutil.ExtractDomain(target)
		if deps.health != nil {
			go deps.health.RecordLatency(routed.Route.NodeHash, domain, nil)
		}
		rawConn, err := routed.Outbound.DialContext(ctx, "tcp", M.ParseSocksaddr(target))
		if err == nil {
			if earlyConn, ok := common.Cast[N.EarlyConn](rawConn); ok && earlyConn.NeedHandshake() {
				_, err = rawConn.Write(nil)
			}
		}
		if err != nil {
			if rawConn != nil {
				_ = rawConn.Close()
			}
			proxyErr := classifyConnectError(err)
			if proxyErr == nil {
				return tunnelPrepareResult{route: routed.Route, canceled: true}
			}
			detail := summarizeUpstreamError(err)
			recordPassiveResultAsync(deps.health, routed.Route, false)
			if shouldRecordTargetEgressFailure(detail) {
				deps.router.RecordTargetEgressFailure(routed.Route, target, detail.Kind)
			}
			if account != "" {
				deps.router.DeleteLeaseIfMatch(
					routed.Route.PlatformID,
					account,
					routed.Route.NodeHash,
					routed.Route.EgressIP,
				)
			}
			if attempt == 0 {
				initialProxyErr = proxyErr
				initialUpstreamErr = err
				excludedEgressIPs[routed.Route.EgressIP] = struct{}{}
				deps.router.RecordRouteFailover(nil)
				logRouteFailover("retry", initialRoute, routing.RouteResult{}, target, detail.Kind)
				continue
			}
			failed := false
			deps.router.RecordRouteFailover(&failed)
			logRouteFailover("failed", initialRoute, routed.Route, target, detail.Kind)
			return tunnelPrepareResult{
				route:         routed.Route,
				proxyErr:      proxyErr,
				upstreamStage: "connect_dial",
				upstreamErr:   err,
			}
		}
		if attempt > 0 {
			succeeded := true
			deps.router.RecordRouteFailover(&succeeded)
			logRouteFailover("success", initialRoute, routed.Route, target, "")
		}

		recordResult := func(ok bool, failureKind string, targetFailure bool) {
			recordPassiveResultAsync(deps.health, routed.Route, ok)
			if ok {
				deps.router.RecordTargetEgressSuccess(routed.Route, target)
				return
			}
			if targetFailure && failureKind != "http_status_error" {
				opened := deps.router.RecordTargetEgressFailure(
					routed.Route,
					target,
					failureKind,
				)
				if opened && account != "" {
					deps.router.DeleteLeaseIfMatch(
						routed.Route.PlatformID,
						account,
						routed.Route.NodeHash,
						routed.Route.EgressIP,
					)
				}
			}
		}

		var upstreamBase net.Conn = rawConn
		if deps.metricsSink != nil {
			deps.metricsSink.OnConnectionLifecycle(ConnectionOutbound, ConnectionOpen)
			upstreamBase = newCountingConn(rawConn, deps.metricsSink)
		}

		upstreamConn := newTLSLatencyConn(upstreamBase, func(latency time.Duration) {
			if deps.health != nil {
				deps.health.RecordLatency(routed.Route.NodeHash, domain, &latency)
			}
		})

		return tunnelPrepareResult{
			route: routed.Route,
			session: &preparedTunnel{
				upstreamConn: upstreamConn,
				recordResult: recordResult,
			},
		}
	}
	return tunnelPrepareResult{route: initialRoute, proxyErr: ErrNoAvailableNodes}
}

func prepareDirectConnectTunnel(ctx context.Context, deps tunnelDeps, target string) tunnelPrepareResult {
	var dialer net.Dialer
	rawConn, err := dialer.DialContext(ctx, "tcp", target)
	if err != nil {
		proxyErr := classifyConnectError(err)
		if proxyErr == nil {
			return tunnelPrepareResult{canceled: true}
		}
		return tunnelPrepareResult{
			proxyErr:      proxyErr,
			upstreamStage: "connect_direct_dial",
			upstreamErr:   err,
		}
	}

	var upstreamConn net.Conn = rawConn
	if deps.metricsSink != nil {
		deps.metricsSink.OnConnectionLifecycle(ConnectionOutbound, ConnectionOpen)
		upstreamConn = newCountingConn(rawConn, deps.metricsSink)
	}
	return tunnelPrepareResult{
		session: &preparedTunnel{
			upstreamConn: upstreamConn,
			recordResult: func(bool, string, bool) {},
		},
	}
}

func logRouteFailover(action string, initial, final routing.RouteResult, target, failureKind string) {
	log.Printf(
		"route_failover action=%s initial_node_hash=%s initial_egress_hash=%s final_node_hash=%s final_egress_hash=%s target_domain=%s failure_kind=%s",
		action,
		initial.NodeHash.Hex(),
		hashRouteEgress(initial.EgressIP),
		final.NodeHash.Hex(),
		hashRouteEgress(final.EgressIP),
		netutil.ExtractDomain(target),
		failureKind,
	)
}

func hashRouteEgress(ip netip.Addr) string {
	if !ip.IsValid() {
		return ""
	}
	sum := sha256.Sum256([]byte(ip.String()))
	return hex.EncodeToString(sum[:6])
}

func pumpPreparedTunnel(
	clientConn net.Conn,
	clientReader *bufio.Reader,
	session *preparedTunnel,
	opts tunnelPumpOptions,
) tunnelRelayResult {
	clientToUpstream, err := makeTunnelClientReader(clientConn, clientReader)
	if err != nil {
		if session != nil && session.upstreamConn != nil {
			_ = session.upstreamConn.Close()
		}
		if clientConn != nil {
			_ = clientConn.Close()
		}
		return tunnelRelayResult{
			proxyErr:      ErrUpstreamRequestFailed,
			upstreamStage: "connect_client_prefetch_drain",
			upstreamErr:   err,
		}
	}
	return pumpPreparedTunnelReader(clientConn, clientToUpstream, session, opts)
}

func pumpPreparedTunnelReader(
	clientConn net.Conn,
	clientToUpstream io.Reader,
	session *preparedTunnel,
	opts tunnelPumpOptions,
) tunnelRelayResult {
	if clientConn == nil || clientToUpstream == nil || session == nil || session.upstreamConn == nil {
		return tunnelRelayResult{}
	}

	type copyResult struct {
		n   int64
		err error
	}
	var closeBothOnce sync.Once
	closeBoth := func() {
		closeBothOnce.Do(func() {
			_ = clientConn.Close()
			_ = session.upstreamConn.Close()
		})
	}
	ingressBytesCh := make(chan copyResult, 1)
	egressBytesCh := make(chan copyResult, 1)
	go func() {
		n, copyErr := io.Copy(session.upstreamConn, clientToUpstream)
		if !isBenignTunnelCopyError(copyErr) || !closeWriteConn(session.upstreamConn) {
			closeBoth()
		}
		egressBytesCh <- copyResult{n: n, err: copyErr}
	}()
	go func() {
		n, copyErr := io.Copy(clientConn, session.upstreamConn)
		if !isBenignTunnelCopyError(copyErr) || !closeWriteConn(clientConn) {
			closeBoth()
		}
		ingressBytesCh <- copyResult{n: n, err: copyErr}
	}()

	ingressResult := <-ingressBytesCh
	egressResult := <-egressBytesCh
	closeBoth()

	ingressErrBenign := isBenignTunnelCopyError(ingressResult.err)
	egressErrBenign := isBenignTunnelCopyError(egressResult.err)
	// A client-side TCP reset after the upstream response has already started is
	// a shutdown artifact, not an upstream failure. This commonly happens when a
	// tunnel client exits immediately after consuming the response.
	if !egressErrBenign && ingressResult.n > 0 && isClientReadResetError(egressResult.err) {
		egressErrBenign = true
	}

	result := tunnelRelayResult{
		ingressBytes: ingressResult.n,
		egressBytes:  egressResult.n,
		netOK:        true,
	}
	switch {
	case !ingressErrBenign:
		result.netOK = false
		result.proxyErr = ErrUpstreamRequestFailed
		result.upstreamStage = "connect_upstream_to_client_copy"
		result.upstreamErr = ingressResult.err
	case !egressErrBenign:
		result.netOK = false
		result.proxyErr = ErrUpstreamRequestFailed
		result.upstreamStage = "connect_client_to_upstream_copy"
		result.upstreamErr = egressResult.err
	case opts.requireBidirectionalTraffic && (ingressResult.n == 0 || egressResult.n == 0):
		result.netOK = false
		result.proxyErr = ErrUpstreamRequestFailed
		switch {
		case ingressResult.n == 0 && egressResult.n == 0:
			result.upstreamStage = "connect_zero_traffic"
		case ingressResult.n == 0:
			result.upstreamStage = "connect_no_ingress_traffic"
		default:
			result.upstreamStage = "connect_no_egress_traffic"
		}
	}
	return result
}

func closeWriteConn(conn net.Conn) bool {
	return closeWriteErr(conn) == nil
}

// makeTunnelClientReader returns a reader for client->upstream copy that
// preserves any bytes already buffered by a protocol reader before tunneling.
func makeTunnelClientReader(clientConn net.Conn, buffered *bufio.Reader) (io.Reader, error) {
	if buffered == nil {
		return clientConn, nil
	}
	n := buffered.Buffered()
	if n == 0 {
		return clientConn, nil
	}
	prefetched := make([]byte, n)
	if _, err := io.ReadFull(buffered, prefetched); err != nil {
		return nil, err
	}
	return io.MultiReader(bytes.NewReader(prefetched), clientConn), nil
}

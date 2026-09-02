package proxy

import (
	"net/netip"

	"github.com/Resinat/Resin/internal/outbound"
	"github.com/Resinat/Resin/internal/routing"
	"github.com/sagernet/sing-box/adapter"
)

type routedOutbound struct {
	Route    routing.RouteResult
	Outbound adapter.Outbound
}

// resolveRoutedOutboundExcluding resolves an outbound while excluding failed egress IPs.
func resolveRoutedOutboundExcluding(
	router *routing.Router,
	pool outbound.PoolAccessor,
	platformName string,
	account string,
	target string,
	excludedEgressIPs map[netip.Addr]struct{},
) (routedOutbound, *ProxyError) {
	var result routing.RouteResult
	var err error
	if len(excludedEgressIPs) == 0 {
		result, err = router.RouteRequest(platformName, account, target)
	} else {
		result, err = router.RouteRequestExcluding(
			platformName,
			account,
			target,
			excludedEgressIPs,
		)
	}
	if err != nil {
		return routedOutbound{}, mapRouteError(err)
	}

	entry, ok := pool.GetEntry(result.NodeHash)
	if !ok {
		return routedOutbound{}, ErrNoAvailableNodes
	}
	obPtr := entry.Outbound.Load()
	if obPtr == nil {
		return routedOutbound{}, ErrNoAvailableNodes
	}
	return routedOutbound{Route: result, Outbound: *obPtr}, nil
}

func resolveRoutedOutbound(
	router *routing.Router,
	pool outbound.PoolAccessor,
	platformName string,
	account string,
	target string,
) (routedOutbound, *ProxyError) {
	return resolveRoutedOutboundExcluding(router, pool, platformName, account, target, nil)
}

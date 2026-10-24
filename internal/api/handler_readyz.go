package api

import (
	"net/http"
	"strings"
	"sync/atomic"

	"github.com/Resinat/Resin/internal/config"
	"github.com/Resinat/Resin/internal/metrics"
)

const (
	defaultReadyMinHealthyNodeRatio   = 0.10
	defaultReadyMinHealthyEgressRatio = 0.20
)

// HandleReadyz reports whether the node pool is ready to receive traffic.
// With platform_id it evaluates the platform's routable view; without it the
// legacy global aggregate is retained.
func HandleReadyz(manager *metrics.Manager, runtimeCfg *atomic.Pointer[config.RuntimeConfig]) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		minHealthyNodeRatio := defaultReadyMinHealthyNodeRatio
		minHealthyEgressRatio := defaultReadyMinHealthyEgressRatio
		if runtimeCfg != nil {
			if cfg := runtimeCfg.Load(); cfg != nil {
				if cfg.ReadyMinHealthyNodeRatio > 0 && cfg.ReadyMinHealthyNodeRatio <= 1 {
					minHealthyNodeRatio = cfg.ReadyMinHealthyNodeRatio
				}
				if cfg.ReadyMinHealthyEgressRatio > 0 && cfg.ReadyMinHealthyEgressRatio <= 1 {
					minHealthyEgressRatio = cfg.ReadyMinHealthyEgressRatio
				}
			}
		}
		stats := metrics.RuntimeStatsProvider(nil)
		if manager != nil {
			stats = manager.RuntimeStats()
		}
		if stats == nil {
			WriteJSON(w, http.StatusServiceUnavailable, map[string]any{
				"status":  "not_ready",
				"reasons": []string{"runtime_stats_unavailable"},
				"thresholds": map[string]float64{
					"healthy_node_ratio":      minHealthyNodeRatio,
					"healthy_egress_ip_ratio": minHealthyEgressRatio,
				},
			})
			return
		}
		platformID := strings.TrimSpace(r.URL.Query().Get("platform_id"))
		scope := "global"
		totalNodes := stats.TotalNodes()
		healthyNodes := stats.HealthyNodes()
		egressIPs := stats.EgressIPCount()
		healthyEgressIPs := stats.UniqueHealthyEgressIPCount()
		if platformID != "" {
			routableNodes, nodesOK := stats.RoutableNodeCount(platformID)
			platformEgressIPs, egressOK := stats.PlatformEgressIPCount(platformID)
			if !nodesOK || !egressOK {
				WriteJSON(w, http.StatusServiceUnavailable, map[string]any{
					"status":      "not_ready",
					"scope":       "platform",
					"platform_id": platformID,
					"reasons":     []string{"platform_not_found"},
					"thresholds": map[string]float64{
						"healthy_node_ratio":      minHealthyNodeRatio,
						"healthy_egress_ip_ratio": minHealthyEgressRatio,
					},
				})
				return
			}
			// RoutableNodeCount and PlatformEgressIPCount are already scoped to
			// the platform's eligible view, so both totals are healthy counts.
			scope = "platform"
			totalNodes, healthyNodes = routableNodes, routableNodes
			egressIPs, healthyEgressIPs = platformEgressIPs, platformEgressIPs
		}
		nodeRatio := 0.0
		egressRatio := 0.0
		if totalNodes > 0 {
			nodeRatio = float64(healthyNodes) / float64(totalNodes)
		}
		if egressIPs > 0 {
			egressRatio = float64(healthyEgressIPs) / float64(egressIPs)
		}
		reasons := make([]string, 0, 2)
		if totalNodes == 0 {
			reasons = append(reasons, "no_nodes")
		} else if nodeRatio < minHealthyNodeRatio {
			reasons = append(reasons, "healthy_node_ratio_below_threshold")
		}
		if egressIPs == 0 {
			reasons = append(reasons, "no_egress_ips")
		} else if egressRatio < minHealthyEgressRatio {
			reasons = append(reasons, "healthy_egress_ratio_below_threshold")
		}
		ready := len(reasons) == 0
		status := http.StatusOK
		state := "ready"
		if !ready {
			status = http.StatusServiceUnavailable
			state = "not_ready"
		}
		WriteJSON(w, status, map[string]any{
			"status":                  state,
			"scope":                   scope,
			"platform_id":             platformID,
			"total_nodes":             totalNodes,
			"healthy_nodes":           healthyNodes,
			"egress_ips":              egressIPs,
			"healthy_egress_ips":      healthyEgressIPs,
			"healthy_node_ratio":      nodeRatio,
			"healthy_egress_ip_ratio": egressRatio,
			"reasons":                 reasons,
			"thresholds": map[string]float64{
				"healthy_node_ratio":      minHealthyNodeRatio,
				"healthy_egress_ip_ratio": minHealthyEgressRatio,
			},
		})
	}
}

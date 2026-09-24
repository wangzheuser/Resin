package api

import (
	"net/http"

	"github.com/Resinat/Resin/internal/metrics"
)

const (
	defaultReadyMinHealthyNodeRatio   = 0.10
	defaultReadyMinHealthyEgressRatio = 0.20
)

// HandleReadyz reports whether the node pool is ready to receive traffic.
// It exposes aggregate counts only; credentials and node details stay private.
func HandleReadyz(manager *metrics.Manager, minHealthyNodeRatio, minHealthyEgressRatio float64) http.HandlerFunc {
	if minHealthyNodeRatio <= 0 || minHealthyNodeRatio > 1 {
		minHealthyNodeRatio = defaultReadyMinHealthyNodeRatio
	}
	if minHealthyEgressRatio <= 0 || minHealthyEgressRatio > 1 {
		minHealthyEgressRatio = defaultReadyMinHealthyEgressRatio
	}

	return func(w http.ResponseWriter, r *http.Request) {
		if manager == nil || manager.RuntimeStats() == nil {
			WriteJSON(w, http.StatusServiceUnavailable, map[string]any{
				"status": "not_ready",
				"reason": "runtime_stats_unavailable",
			})
			return
		}

		stats := manager.RuntimeStats()
		totalNodes := stats.TotalNodes()
		healthyNodes := stats.HealthyNodes()
		egressIPs := stats.EgressIPCount()
		healthyEgressIPs := stats.UniqueHealthyEgressIPCount()
		nodeRatio := 0.0
		egressRatio := 0.0
		if totalNodes > 0 {
			nodeRatio = float64(healthyNodes) / float64(totalNodes)
		}
		if egressIPs > 0 {
			egressRatio = float64(healthyEgressIPs) / float64(egressIPs)
		}
		ready := totalNodes > 0 && healthyNodes > 0 &&
			nodeRatio >= minHealthyNodeRatio &&
			egressIPs > 0 && egressRatio >= minHealthyEgressRatio
		status := http.StatusOK
		state := "ready"
		if !ready {
			status = http.StatusServiceUnavailable
			state = "not_ready"
		}
		WriteJSON(w, status, map[string]any{
			"status":                  state,
			"total_nodes":             totalNodes,
			"healthy_nodes":           healthyNodes,
			"egress_ips":              egressIPs,
			"healthy_egress_ips":      healthyEgressIPs,
			"healthy_node_ratio":      nodeRatio,
			"healthy_egress_ip_ratio": egressRatio,
		})
	}
}

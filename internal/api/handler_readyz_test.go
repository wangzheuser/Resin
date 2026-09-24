package api

import (
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/Resinat/Resin/internal/metrics"
)

type readyStats struct {
	total, healthy, egress, healthyEgress int
}

func (s readyStats) TotalNodes() int                          { return s.total }
func (s readyStats) HealthyNodes() int                        { return s.healthy }
func (s readyStats) EgressIPCount() int                       { return s.egress }
func (s readyStats) UniqueHealthyEgressIPCount() int          { return s.healthyEgress }
func (s readyStats) LeaseCountsByPlatform() map[string]int    { return nil }
func (s readyStats) RoutableNodeCount(string) (int, bool)     { return 0, false }
func (s readyStats) PlatformEgressIPCount(string) (int, bool) { return 0, false }
func (s readyStats) CollectNodeEWMAs(string) []float64        { return nil }

func newReadyManager(stats metrics.RuntimeStatsProvider) *metrics.Manager {
	return metrics.NewManager(metrics.ManagerConfig{RuntimeStats: stats})
}

func TestReadyz_Returns503WhenPoolDegraded(t *testing.T) {
	h := HandleReadyz(newReadyManager(readyStats{total: 100, healthy: 4, egress: 20, healthyEgress: 18}), 0.10, 0.20)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/readyz", nil))
	if rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("status: got %d, want %d", rec.Code, http.StatusServiceUnavailable)
	}
}

func TestReadyz_Returns200WhenPoolMeetsThresholds(t *testing.T) {
	h := HandleReadyz(newReadyManager(readyStats{total: 100, healthy: 20, egress: 20, healthyEgress: 10}), 0.10, 0.20)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/readyz", nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("status: got %d, want %d", rec.Code, http.StatusOK)
	}
}

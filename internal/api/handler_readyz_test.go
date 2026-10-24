package api

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/Resinat/Resin/internal/config"
	"github.com/Resinat/Resin/internal/metrics"
)

type readyStats struct {
	total, healthy, egress, healthyEgress int
	platformNodes                         map[string]int
	platformEgress                        map[string]int
}

func (s readyStats) TotalNodes() int                       { return s.total }
func (s readyStats) HealthyNodes() int                     { return s.healthy }
func (s readyStats) EgressIPCount() int                    { return s.egress }
func (s readyStats) UniqueHealthyEgressIPCount() int       { return s.healthyEgress }
func (s readyStats) LeaseCountsByPlatform() map[string]int { return nil }
func (s readyStats) RoutableNodeCount(platformID string) (int, bool) {
	v, ok := s.platformNodes[platformID]
	return v, ok
}
func (s readyStats) PlatformEgressIPCount(platformID string) (int, bool) {
	v, ok := s.platformEgress[platformID]
	return v, ok
}
func (s readyStats) CollectNodeEWMAs(string) []float64 { return nil }

func newReadyManager(stats metrics.RuntimeStatsProvider) *metrics.Manager {
	return metrics.NewManager(metrics.ManagerConfig{RuntimeStats: stats})
}

func TestReadyz_Returns503WhenPoolDegraded(t *testing.T) {
	cfg := &atomic.Pointer[config.RuntimeConfig]{}
	cfg.Store(config.NewDefaultRuntimeConfig())
	h := HandleReadyz(newReadyManager(readyStats{total: 100, healthy: 4, egress: 20, healthyEgress: 18}), cfg)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/readyz", nil))
	if rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("status: got %d, want %d", rec.Code, http.StatusServiceUnavailable)
	}
}

func TestReadyz_Returns200WhenPoolMeetsThresholds(t *testing.T) {
	cfg := &atomic.Pointer[config.RuntimeConfig]{}
	cfg.Store(config.NewDefaultRuntimeConfig())
	h := HandleReadyz(newReadyManager(readyStats{total: 100, healthy: 20, egress: 20, healthyEgress: 10}), cfg)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/readyz", nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("status: got %d, want %d", rec.Code, http.StatusOK)
	}
}

func TestReadyz_UsesUpdatedThresholdsWithoutRestart(t *testing.T) {
	cfg := &atomic.Pointer[config.RuntimeConfig]{}
	initial := config.NewDefaultRuntimeConfig()
	initial.ReadyMinHealthyNodeRatio = 0.10
	initial.ReadyMinHealthyEgressRatio = 0.20
	cfg.Store(initial)
	h := HandleReadyz(newReadyManager(readyStats{total: 100, healthy: 8, egress: 20, healthyEgress: 10}), cfg)

	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/readyz", nil))
	if rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("initial status: got %d, want %d", rec.Code, http.StatusServiceUnavailable)
	}

	updated := *initial
	updated.ReadyMinHealthyNodeRatio = 0.05
	cfg.Store(&updated)
	rec = httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/readyz", nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("updated status: got %d, want %d", rec.Code, http.StatusOK)
	}
}

func TestReadyz_UsesPlatformScopeWhenRequested(t *testing.T) {
	cfg := &atomic.Pointer[config.RuntimeConfig]{}
	cfg.Store(config.NewDefaultRuntimeConfig())
	h := HandleReadyz(newReadyManager(readyStats{
		total: 100, healthy: 1, egress: 20, healthyEgress: 1,
		platformNodes:  map[string]int{"node": 20},
		platformEgress: map[string]int{"node": 10},
	}), cfg)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/readyz?platform_id=node", nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("platform status: got %d, want %d; body=%s", rec.Code, http.StatusOK, rec.Body.String())
	}
	if !strings.Contains(rec.Body.String(), `"scope":"platform"`) || !strings.Contains(rec.Body.String(), `"platform_id":"node"`) {
		t.Fatalf("platform scope missing: %s", rec.Body.String())
	}
}

func TestReadyz_UnknownPlatformIsNotReady(t *testing.T) {
	cfg := &atomic.Pointer[config.RuntimeConfig]{}
	cfg.Store(config.NewDefaultRuntimeConfig())
	h := HandleReadyz(newReadyManager(readyStats{}), cfg)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/readyz?platform_id=missing", nil))
	if rec.Code != http.StatusServiceUnavailable || !strings.Contains(rec.Body.String(), `platform_not_found`) {
		t.Fatalf("unknown platform response: status=%d body=%s", rec.Code, rec.Body.String())
	}
}

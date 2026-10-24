package api

import (
	"net/http"
	"testing"

	"github.com/Resinat/Resin/internal/probe"
	"github.com/Resinat/Resin/internal/subscription"
	"github.com/google/uuid"
)

func TestAPIContract_SubscriptionProbe(t *testing.T) {
	srv, cp, _ := newControlPlaneTestServer(t)
	id := uuid.NewString()
	cp.SubMgr.Register(subscription.NewSubscription(id, "probe", "https://example.com", true, false))
	cp.ProbeMgr = probe.NewProbeManager(probe.ProbeConfig{Pool: cp.Pool, Concurrency: 1})
	cp.ProbeMgr.Start()
	t.Cleanup(cp.ProbeMgr.Stop)
	for _, path := range []string{"/api/v1/subscriptions/probe-job", "/api/v1/subscriptions/" + id + "/actions/probe-egress"} {
		method := http.MethodGet
		if path != "/api/v1/subscriptions/probe-job" {
			method = http.MethodPost
		}
		if rec := doJSONRequest(t, srv, method, path, nil, false); rec.Code != http.StatusUnauthorized {
			t.Fatalf("auth: %d", rec.Code)
		}
	}
	rec := doJSONRequest(t, srv, http.MethodGet, "/api/v1/subscriptions/probe-job", nil, true)
	if rec.Code != http.StatusOK || rec.Body.String() != "null\n" {
		t.Fatalf("initial job: %d %s", rec.Code, rec.Body.String())
	}
	rec = doJSONRequest(t, srv, http.MethodPost, "/api/v1/subscriptions/"+id+"/actions/probe-egress", nil, true)
	if rec.Code != http.StatusAccepted {
		t.Fatalf("start: %d %s", rec.Code, rec.Body.String())
	}
	jobID := decodeJSONMap(t, rec)["id"].(string)
	rec = doJSONRequest(t, srv, http.MethodDelete, "/api/v1/subscriptions/probe-job/"+jobID, nil, true)
	if rec.Code != http.StatusOK {
		t.Fatalf("cancel: %d %s", rec.Code, rec.Body.String())
	}
	if rec := doJSONRequest(t, srv, http.MethodPost, "/api/v1/subscriptions/invalid/actions/probe-egress", nil, true); rec.Code != http.StatusBadRequest {
		t.Fatal("invalid ID accepted")
	}
}

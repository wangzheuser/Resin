package api

import (
	"context"
	"net/http"
	"strings"
	"testing"

	"github.com/Resinat/Resin/internal/netutil"
	"github.com/Resinat/Resin/internal/topology"
)

func TestSubscriptionDownloadAPIReportsFailures(t *testing.T) {
	for _, tc := range []struct {
		name   string
		err    error
		body   []byte
		status int
	}{
		{"timeout", context.DeadlineExceeded, nil, 504},
		{"upstream", &netutil.HTTPStatusError{StatusCode: 400, URL: "https://example.com/private?token=secret"}, nil, 502},
		{"no_route", &netutil.SubscriptionFetchError{Kind: "no_route", Message: "no eligible route", Retryable: true}, nil, 503},
		{"parse", nil, []byte("invalid content"), 502},
	} {
		t.Run(tc.name, func(t *testing.T) {
			srv, cp, _ := newControlPlaneTestServer(t)
			cp.Scheduler = topology.NewSubscriptionScheduler(topology.SchedulerConfig{SubManager: cp.SubMgr, Pool: cp.Pool, Fetcher: func(string) ([]byte, error) { return tc.body, tc.err }})
			defer cp.Scheduler.Stop()
			created := doJSONRequest(t, srv, http.MethodPost, "/api/v1/subscriptions", map[string]any{"name": "download-test", "url": "https://example.com/sub", "download_mode": "direct"}, true)
			if created.Code != 201 {
				t.Fatal(created.Body.String())
			}
			data := decodeJSONMap(t, created)
			id := data["id"].(string)
			if data["download_mode"] != "direct" || data["update_status"] != "never" {
				t.Fatal("incorrect initial status")
			}
			result := doJSONRequest(t, srv, http.MethodPost, "/api/v1/subscriptions/"+id+"/actions/refresh", nil, true)
			if result.Code != tc.status || strings.Contains(result.Body.String(), "secret") {
				t.Fatalf("status=%d body=%s", result.Code, result.Body.String())
			}
			state := decodeJSONMap(t, doJSONRequest(t, srv, http.MethodGet, "/api/v1/subscriptions/"+id, nil, true))
			if state["consecutive_failures"] != float64(1) || state["next_refresh"] == nil {
				t.Fatal("missing retry metadata")
			}
		})
	}
}

func TestSubscriptionDownloadPlatformReference(t *testing.T) {
	srv, _, _ := newControlPlaneTestServer(t)
	created := doJSONRequest(t, srv, http.MethodPost, "/api/v1/platforms", map[string]any{"name": "download-route"}, true)
	if created.Code != 201 {
		t.Fatal(created.Body.String())
	}
	id := decodeJSONMap(t, created)["id"].(string)
	sub := doJSONRequest(t, srv, http.MethodPost, "/api/v1/subscriptions", map[string]any{"name": "referenced", "url": "https://example.com/sub", "download_mode": "platform", "download_platform_id": id}, true)
	if sub.Code != 201 {
		t.Fatal(sub.Body.String())
	}
	deleted := doJSONRequest(t, srv, http.MethodDelete, "/api/v1/platforms/"+id, nil, true)
	if deleted.Code != 409 {
		t.Fatalf("referenced platform deletion status=%d", deleted.Code)
	}
	invalid := doJSONRequest(t, srv, http.MethodPost, "/api/v1/subscriptions", map[string]any{"name": "invalid", "url": "https://example.com/sub", "download_mode": "platform"}, true)
	if invalid.Code != 400 {
		t.Fatal("missing platform accepted")
	}
}

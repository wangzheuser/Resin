package probe

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/Resinat/Resin/internal/node"
)

func TestDirectFetcher_HTTPFallbackLatencyNonZero(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		time.Sleep(15 * time.Millisecond)
		_, _ = w.Write([]byte("ok"))
	}))
	defer srv.Close()

	fetcher := DirectFetcher(func() time.Duration { return 10 * time.Second })
	body, latency, err := fetcher(node.Zero, srv.URL)
	if err != nil {
		t.Fatalf("fetch failed: %v", err)
	}
	if string(body) != "ok" {
		t.Fatalf("body: got %q, want %q", string(body), "ok")
	}
	if latency <= 0 {
		t.Fatalf("latency should be > 0, got %v", latency)
	}
}

func TestDirectFetcher_RejectsOversizedResponse(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(strings.Repeat("x", int(maxProbeResponseBytes)+1)))
	}))
	defer srv.Close()

	fetcher := DirectFetcher(func() time.Duration { return time.Second })
	_, _, err := fetcher(node.Zero, srv.URL)
	if err == nil || !strings.Contains(err.Error(), "response exceeds") {
		t.Fatalf("expected bounded-response error, got %v", err)
	}
}

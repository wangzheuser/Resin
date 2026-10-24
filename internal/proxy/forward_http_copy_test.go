package proxy

import (
	"context"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"net/http/httptrace"
	"strings"
	"testing"
)

func TestCanRetryForwardRequest(t *testing.T) {
	failedBeforeWrite := newUpstreamRequestTrace()
	if !canRetryForwardRequest(httptest.NewRequest(http.MethodGet, "http://example.com", nil), failedBeforeWrite) {
		t.Fatal("GET that failed before upstream write should be retryable")
	}

	wrote := newUpstreamRequestTrace()
	wrote.clientTrace().GotConn(httptrace.GotConnInfo{})
	if canRetryForwardRequest(httptest.NewRequest(http.MethodGet, "http://example.com", nil), wrote) {
		t.Fatal("partial request writes must not be replayed")
	}
	wrote.clientTrace().WroteRequest(httptrace.WroteRequestInfo{})
	if canRetryForwardRequest(httptest.NewRequest(http.MethodGet, "http://example.com", nil), wrote) {
		t.Fatal("request reported as written must not be replayed")
	}

	post := httptest.NewRequest(http.MethodPost, "http://example.com", io.NopCloser(strings.NewReader("body")))
	post.ContentLength = 4
	if canRetryForwardRequest(post, failedBeforeWrite) {
		t.Fatal("POST with a body must not be replayed")
	}
}

func TestShouldRecordForwardCopyFailure(t *testing.T) {
	t.Run("NilError", func(t *testing.T) {
		req := httptest.NewRequest("GET", "http://example.com", nil)
		if shouldRecordForwardCopyFailure(req, nil) {
			t.Fatal("nil copy error should not be recorded as failure")
		}
	})

	t.Run("ClientCanceledContext", func(t *testing.T) {
		ctx, cancel := context.WithCancel(context.Background())
		cancel()
		req := httptest.NewRequest("GET", "http://example.com", nil).WithContext(ctx)
		if shouldRecordForwardCopyFailure(req, genericErr{}) {
			t.Fatal("client-canceled request should not penalize node health")
		}
	})

	t.Run("ContextCanceledError", func(t *testing.T) {
		req := httptest.NewRequest("GET", "http://example.com", nil)
		if shouldRecordForwardCopyFailure(req, canceledErr{}) {
			t.Fatal("context canceled errors should not penalize node health")
		}
	})

	t.Run("UpstreamTimeout", func(t *testing.T) {
		req := httptest.NewRequest("GET", "http://example.com", nil)
		if !shouldRecordForwardCopyFailure(req, deadlineExceededErr{}) {
			t.Fatal("upstream timeout should be recorded as node failure")
		}
	})

	t.Run("GenericUpstreamError", func(t *testing.T) {
		req := httptest.NewRequest("GET", "http://example.com", nil)
		err := errors.New("upstream read failed")
		if !shouldRecordForwardCopyFailure(req, err) {
			t.Fatal("generic upstream read error should be recorded as node failure")
		}
	})
}

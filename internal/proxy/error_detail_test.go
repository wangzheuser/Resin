package proxy

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"os"
	"strings"
	"syscall"
	"testing"

	ws "github.com/sagernet/ws"
)

func TestSummarizeUpstreamError_HTTPStatus(t *testing.T) {
	for _, status := range []int{403, 404, 429, 500} {
		t.Run(fmt.Sprintf("status_%d", status), func(t *testing.T) {
			err := fmt.Errorf("dial websocket: %w", ws.StatusError(status))
			detail := summarizeUpstreamError(err)
			if detail.Kind != "http_status_error" {
				t.Fatalf("kind: got %q, want %q", detail.Kind, "http_status_error")
			}
			wantErrno := fmt.Sprintf("HTTP_%d", status)
			if detail.Errno != wantErrno {
				t.Fatalf("errno: got %q, want %q", detail.Errno, wantErrno)
			}
		})
	}
}

func TestSummarizeUpstreamError_Canceled(t *testing.T) {
	detail := summarizeUpstreamError(context.Canceled)
	if detail.Kind != "canceled" {
		t.Fatalf("kind: got %q, want %q", detail.Kind, "canceled")
	}
	if detail.Message == "" {
		t.Fatal("expected non-empty message")
	}
}

func TestSummarizeUpstreamError_DNS(t *testing.T) {
	err := &net.OpError{Op: "dial", Err: &net.DNSError{Err: "no such host"}}
	detail := summarizeUpstreamError(err)
	if detail.Kind != "dns_error" {
		t.Fatalf("kind: got %q, want %q", detail.Kind, "dns_error")
	}
}

func TestSummarizeUpstreamError_Errno(t *testing.T) {
	err := &os.SyscallError{Syscall: "connect", Err: syscall.ECONNREFUSED}
	detail := summarizeUpstreamError(err)
	if detail.Kind != "connection_refused" {
		t.Fatalf("kind: got %q, want %q", detail.Kind, "connection_refused")
	}
	if detail.Errno != "ECONNREFUSED" {
		t.Fatalf("errno: got %q, want %q", detail.Errno, "ECONNREFUSED")
	}
}

func TestSanitizeUpstreamErrMsg_TruncatesAndNormalizes(t *testing.T) {
	raw := strings.Repeat("x", maxUpstreamErrMsgLen+20) + "\n\n"
	got := sanitizeUpstreamErrMsg(raw)
	if len(got) != maxUpstreamErrMsgLen {
		t.Fatalf("len: got %d, want %d", len(got), maxUpstreamErrMsgLen)
	}
	if strings.Contains(got, "\n") {
		t.Fatal("expected normalized single-line message")
	}
}

func TestIsBenignTunnelCopyError(t *testing.T) {
	cases := []struct {
		name string
		err  error
		want bool
	}{
		{name: "nil", err: nil, want: true},
		{name: "eof", err: io.EOF, want: true},
		{name: "net-closed", err: net.ErrClosed, want: true},
		{name: "context-canceled", err: context.Canceled, want: true},
		{name: "closed-network-connection", err: errors.New("write tcp: use of closed network connection"), want: true},
		{name: "generic", err: errors.New("connection reset by peer"), want: false},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := isBenignTunnelCopyError(tc.err)
			if got != tc.want {
				t.Fatalf("got %v, want %v", got, tc.want)
			}
		})
	}
}

func TestIsClientReadResetError(t *testing.T) {
	cases := []string{
		"read tcp: connection reset by peer",
		"read tcp: software caused connection abort",
		"wsarecv: An existing connection was forcibly closed by the remote host.",
		"wsarecv: An established connection was aborted by the software in your host machine.",
	}
	for _, message := range cases {
		if !isClientReadResetError(errors.New(message)) {
			t.Fatalf("expected client reset error: %q", message)
		}
	}
}

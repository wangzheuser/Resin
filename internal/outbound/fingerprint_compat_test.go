//go:build with_utls

package outbound

import (
	"bytes"
	"encoding/json"
	"io"
	"strings"
	"testing"
)

func TestSingboxBuilder_UnsupportedFingerprintFallback(t *testing.T) {
	b := newTestSingboxBuilder(t)
	t.Cleanup(func() { _ = b.Close() })
	for _, fp := range []string{"unsafe", "none", "unknown-fixture", "chrome"} {
		t.Run(fp, func(t *testing.T) {
			cfg := map[string]any{"type": "vless", "server": "127.0.0.1", "server_port": 1,
				"uuid": "00000000-0000-4000-8000-000000000001", "transport": map[string]any{"type": "ws", "path": "/"},
				"tls": map[string]any{"enabled": true, "server_name": "example.invalid", "utls": map[string]any{"enabled": true, "fingerprint": fp}}}
			raw, _ := json.Marshal(cfg)
			original := append([]byte(nil), raw...)
			ob, err := b.Build(raw)
			if err != nil {
				t.Fatalf("ordinary TLS should build: %v", err)
			}
			if closer, ok := ob.(io.Closer); ok {
				defer closer.Close()
			}
			if !bytes.Equal(raw, original) {
				t.Fatal("stored options changed")
			}
		})
	}
	raw := []byte(`{"type":"vless","server":"127.0.0.1","server_port":1,"uuid":"00000000-0000-4000-8000-000000000001","tls":{"enabled":true,"server_name":"example.invalid","utls":{"enabled":true,"fingerprint":"unsafe"},"reality":{"enabled":true,"public_key":"AAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAA"}}}`)
	if _, err := b.Build(raw); err == nil || !strings.Contains(err.Error(), "unknown uTLS fingerprint") {
		t.Fatalf("Reality must retain strict fingerprint validation, got %v", err)
	}
}

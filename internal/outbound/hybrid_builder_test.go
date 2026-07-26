package outbound

import (
	"encoding/json"
	"testing"

	"github.com/Resinat/Resin/internal/testutil"
)

func TestHybridBuilderDelegatesNonXHTTP(t *testing.T) {
	builder := NewHybridBuilder(&testutil.StubOutboundBuilder{})
	outbound, err := builder.Build(json.RawMessage(`{"type":"vless","tag":"tcp-node"}`))
	if err != nil {
		t.Fatal(err)
	}
	if got := outbound.Type(); got != "stub" {
		t.Fatalf("delegated outbound type: got %q, want stub", got)
	}
}

func TestHybridBuilderBuildsVLESSXHTTP(t *testing.T) {
	builder := NewHybridBuilder(&testutil.StubOutboundBuilder{})
	raw := json.RawMessage(`{
		"type":"vless",
		"tag":"xhttp-node",
		"server":"example.com",
		"server_port":443,
		"uuid":"00000000-0000-0000-0000-000000000015",
		"flow":"xtls-rprx-vision",
		"encryption":"mlkem768x25519plus.native.0rtt.100-111-1111.75-0-111.50-0-3333.44h-AOrD2oz2uOMssTdsXsdi5lqQFh9xtNeIVqU7mWA",
		"udp":true,
		"tls":{"enabled":true,"server_name":"tls.example.com","alpn":["h2"],"utls":{"enabled":true,"fingerprint":"firefox"}},
		"transport":{"type":"xhttp","path":"/zones","host":"tls.example.com","mode":"stream-up","x_padding_bytes":"100-1000","sc_max_each_post_bytes":"1000000","sc_min_posts_interval_ms":"5","reuse":{"max_concurrency":"1","max_connections":"0"}}
	}`)

	outbound, err := builder.Build(raw)
	if err != nil {
		t.Fatal(err)
	}
	defer closeOutbound(outbound)
	if got := outbound.Type(); got != "vless" {
		t.Fatalf("outbound type: got %q, want vless", got)
	}
	if got := outbound.Tag(); got != "xhttp-node" {
		t.Fatalf("outbound tag: got %q, want xhttp-node", got)
	}
	if got := outbound.Network(); len(got) != 2 || got[0] != "tcp" || got[1] != "udp" {
		t.Fatalf("outbound network: got %v", got)
	}
}

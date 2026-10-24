package outbound

import (
	"reflect"
	"testing"

	"github.com/sagernet/sing-box/option"
)

func TestTLSFingerprintFallbackPreservesVerification(t *testing.T) {
	for _, insecure := range []bool{false, true} {
		original := &option.OutboundTLSOptions{Enabled: true, ServerName: "example.invalid", Insecure: insecure,
			ALPN: []string{"h2", "http/1.1"}, Certificate: []string{"fixture-cert"},
			UTLS: &option.OutboundUTLSOptions{Enabled: true, Fingerprint: "unsafe"}}
		options := &option.OutboundTLSOptionsContainer{TLS: original}
		if !normalizeTLSFingerprint(options) {
			t.Fatal("invalid ordinary fingerprint not handled")
		}
		expected := *original
		expected.UTLS = nil
		if !reflect.DeepEqual(options.TLS, &expected) || original.UTLS == nil {
			t.Fatal("verification options or input changed")
		}
	}
}

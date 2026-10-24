package outbound

import "github.com/sagernet/sing-box/option"

// normalizeTLSFingerprint mirrors the ordinary-TLS fallback used by Mihomo.
// Apply it to parsed runtime options so persisted nodes retain their identity,
// source configuration and certificate verification settings. Reality requires
// uTLS and must keep the core's strict validation.
func normalizeTLSFingerprint(options any) bool {
	wrapper, ok := options.(option.OutboundTLSOptionsWrapper)
	if !ok {
		return false
	}
	tls := wrapper.TakeOutboundTLSOptions()
	if tls == nil || !tls.Enabled || tls.UTLS == nil || !tls.UTLS.Enabled ||
		(tls.Reality != nil && tls.Reality.Enabled) {
		return false
	}
	// Supported by the pinned sing-box 1.12.21 uTLSClientHelloID implementation.
	switch tls.UTLS.Fingerprint {
	case "", "chrome", "chrome_psk", "chrome_psk_shuffle", "chrome_padding_psk_shuffle", "chrome_pq", "chrome_pq_psk",
		"firefox", "edge", "safari", "360", "qq", "ios", "android", "random", "randomized":
		return false
	}
	copyTLS := *tls
	copyTLS.UTLS = nil
	wrapper.ReplaceOutboundTLSOptions(&copyTLS)
	return true
}

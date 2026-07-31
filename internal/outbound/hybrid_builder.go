package outbound

import (
	"context"
	"encoding/json"
	"fmt"
	"net"
	"net/netip"
	"strings"

	mihomoOutbound "github.com/metacubex/mihomo/adapter/outbound"
	MC "github.com/metacubex/mihomo/constant"
	"github.com/sagernet/sing-box/adapter"
	M "github.com/sagernet/sing/common/metadata"
)

// HybridBuilder keeps the existing sing-box path and dispatches only VLESS
// XHTTP nodes to mihomo, whose transport implementation covers XHTTP.
type HybridBuilder struct {
	fallback OutboundBuilder
}

func NewHybridBuilder(fallback OutboundBuilder) *HybridBuilder {
	return &HybridBuilder{fallback: fallback}
}

func (b *HybridBuilder) Build(rawOptions json.RawMessage, relayPlatformIDs ...string) (adapter.Outbound, error) {
	relayPlatformID := firstRelayPlatformID(relayPlatformIDs)
	var header struct {
		Type      string `json:"type"`
		Transport struct {
			Type string `json:"type"`
		} `json:"transport"`
	}
	if err := json.Unmarshal(rawOptions, &header); err != nil ||
		header.Type != "vless" || header.Transport.Type != "xhttp" {
		return b.fallback.Build(rawOptions, relayPlatformID)
	}

	var cfg xhttpOutboundConfig
	if err := json.Unmarshal(rawOptions, &cfg); err != nil {
		return nil, fmt.Errorf("parse xhttp outbound: %w", err)
	}
	if cfg.Server == "" || cfg.ServerPort == 0 || cfg.UUID == "" {
		return nil, fmt.Errorf("parse xhttp outbound: server, server_port and uuid are required")
	}

	option := mihomoOutbound.VlessOption{
		Name:              cfg.Tag,
		Server:            cfg.Server,
		Port:              cfg.ServerPort,
		UUID:              cfg.UUID,
		Flow:              cfg.Flow,
		TLS:               cfg.TLS.Enabled,
		ALPN:              cfg.TLS.ALPN,
		UDP:               cfg.UDP,
		Encryption:        cfg.Encryption,
		Network:           "xhttp",
		SkipCertVerify:    cfg.TLS.Insecure,
		ServerName:        cfg.TLS.ServerName,
		ClientFingerprint: cfg.TLS.UTLS.Fingerprint,
		XHTTPOpts: mihomoOutbound.XHTTPOptions{
			Path:                 cfg.Transport.Path,
			Host:                 cfg.Transport.Host,
			Mode:                 cfg.Transport.Mode,
			Headers:              cfg.Transport.Headers,
			NoGRPCHeader:         cfg.Transport.NoGRPCHeader,
			XPaddingBytes:        cfg.Transport.XPaddingBytes,
			ScMaxEachPostBytes:   cfg.Transport.ScMaxEachPostBytes,
			ScMinPostsIntervalMs: cfg.Transport.ScMinPostsIntervalMs,
		},
	}
	option.TFO = cfg.TCPFastOpen
	if relayPlatformID != "" {
		provider, ok := b.fallback.(interface {
			RelayDialer(string) (*platformRelayDialer, error)
		})
		if !ok {
			return nil, fmt.Errorf("xhttp relay platform runtime is not configured")
		}
		relayDialer, err := provider.RelayDialer(relayPlatformID)
		if err != nil {
			return nil, err
		}
		option.DialerForAPI = &mihomoRelayDialer{dialer: relayDialer}
	}
	if reuse := cfg.Transport.Reuse; reuse != nil {
		option.XHTTPOpts.ReuseSettings = &mihomoOutbound.XHTTPReuseSettings{
			MaxConcurrency:   reuse.MaxConcurrency,
			MaxConnections:   reuse.MaxConnections,
			CMaxReuseTimes:   reuse.CMaxReuseTimes,
			HMaxRequestTimes: reuse.HMaxRequestTimes,
			HMaxReusableSecs: reuse.HMaxReusableSecs,
			HKeepAlivePeriod: reuse.HKeepAlivePeriod,
		}
	}

	client, err := mihomoOutbound.NewVless(option)
	if err != nil {
		return nil, fmt.Errorf("build xhttp outbound: %w", err)
	}
	return &mihomoXHTTPOutbound{tag: cfg.Tag, client: client, udp: cfg.UDP}, nil
}

type xhttpOutboundConfig struct {
	Tag         string `json:"tag"`
	Server      string `json:"server"`
	ServerPort  int    `json:"server_port"`
	UUID        string `json:"uuid"`
	Flow        string `json:"flow"`
	Encryption  string `json:"encryption"`
	UDP         bool   `json:"udp"`
	TCPFastOpen bool   `json:"tcp_fast_open"`
	TLS         struct {
		Enabled    bool     `json:"enabled"`
		ServerName string   `json:"server_name"`
		Insecure   bool     `json:"insecure"`
		ALPN       []string `json:"alpn"`
		UTLS       struct {
			Fingerprint string `json:"fingerprint"`
		} `json:"utls"`
	} `json:"tls"`
	Transport struct {
		Path                 string            `json:"path"`
		Host                 string            `json:"host"`
		Mode                 string            `json:"mode"`
		Headers              map[string]string `json:"headers"`
		NoGRPCHeader         bool              `json:"no_grpc_header"`
		XPaddingBytes        string            `json:"x_padding_bytes"`
		ScMaxEachPostBytes   string            `json:"sc_max_each_post_bytes"`
		ScMinPostsIntervalMs string            `json:"sc_min_posts_interval_ms"`
		Reuse                *struct {
			MaxConcurrency   string `json:"max_concurrency"`
			MaxConnections   string `json:"max_connections"`
			CMaxReuseTimes   string `json:"c_max_reuse_times"`
			HMaxRequestTimes string `json:"h_max_request_times"`
			HMaxReusableSecs string `json:"h_max_reusable_secs"`
			HKeepAlivePeriod int    `json:"h_keep_alive_period"`
		} `json:"reuse"`
	} `json:"transport"`
}

type mihomoXHTTPOutbound struct {
	tag    string
	client *mihomoOutbound.Vless
	udp    bool
}

func (o *mihomoXHTTPOutbound) Type() string           { return "vless" }
func (o *mihomoXHTTPOutbound) Tag() string            { return o.tag }
func (o *mihomoXHTTPOutbound) Dependencies() []string { return nil }

func (o *mihomoXHTTPOutbound) Network() []string {
	if o.udp {
		return []string{"tcp", "udp"}
	}
	return []string{"tcp"}
}

func (o *mihomoXHTTPOutbound) DialContext(ctx context.Context, network string, destination M.Socksaddr) (net.Conn, error) {
	if !strings.HasPrefix(network, "tcp") {
		return nil, fmt.Errorf("xhttp outbound: unsupported network %q", network)
	}
	return o.client.DialContext(ctx, mihomoMetadata(MC.TCP, destination))
}

func (o *mihomoXHTTPOutbound) ListenPacket(ctx context.Context, destination M.Socksaddr) (net.PacketConn, error) {
	if !o.udp {
		return nil, fmt.Errorf("xhttp outbound: udp is disabled")
	}
	return o.client.ListenPacketContext(ctx, mihomoMetadata(MC.UDP, destination))
}

func (o *mihomoXHTTPOutbound) Close() error { return o.client.Close() }

func mihomoMetadata(network MC.NetWork, destination M.Socksaddr) *MC.Metadata {
	metadata := &MC.Metadata{
		NetWork: network,
		Type:    MC.INNER,
		DstPort: destination.Port,
	}
	if destination.IsFqdn() {
		metadata.Host = destination.Fqdn
	} else {
		metadata.DstIP = destination.Addr.Unmap()
	}
	return metadata
}

type mihomoRelayDialer struct {
	dialer *platformRelayDialer
}

// DialContext adapts mihomo's host:port dialer contract to sing metadata.
func (d *mihomoRelayDialer) DialContext(ctx context.Context, network, address string) (net.Conn, error) {
	return d.dialer.DialContext(ctx, network, M.ParseSocksaddr(address))
}

// ListenPacket adapts mihomo's packet dialer contract to the relay selector.
func (d *mihomoRelayDialer) ListenPacket(
	ctx context.Context,
	_ string,
	_ string,
	rAddrPort netip.AddrPort,
) (net.PacketConn, error) {
	return d.dialer.ListenPacket(ctx, M.SocksaddrFromNetIP(rAddrPort))
}

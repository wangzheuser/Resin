package outbound

import (
	"context"
	"encoding/json"
	"errors"
	"net"
	"net/netip"
	"sync/atomic"
	"testing"
	"time"

	"github.com/Resinat/Resin/internal/config"
	"github.com/Resinat/Resin/internal/node"
	"github.com/Resinat/Resin/internal/platform"
	"github.com/sagernet/sing-box/adapter"
	M "github.com/sagernet/sing/common/metadata"
	"github.com/sagernet/sing/common/network"
	ws "github.com/sagernet/ws"
)

type relayTestPool struct {
	entries   map[node.Hash]*node.NodeEntry
	platforms map[string]*platform.Platform
}

func (p *relayTestPool) GetEntry(hash node.Hash) (*node.NodeEntry, bool) {
	entry, ok := p.entries[hash]
	return entry, ok
}

func (p *relayTestPool) GetPlatform(id string) (*platform.Platform, bool) {
	value, ok := p.platforms[id]
	return value, ok
}

type relayTestOutbound struct {
	tag                   string
	networks              []string
	fail                  atomic.Bool
	dials                 atomic.Int32
	dial                  func(context.Context) (net.Conn, error)
	lastPacketDestination M.Socksaddr
}

func (o *relayTestOutbound) Type() string           { return "test" }
func (o *relayTestOutbound) Tag() string            { return o.tag }
func (o *relayTestOutbound) Network() []string      { return o.networks }
func (o *relayTestOutbound) Dependencies() []string { return nil }

func (o *relayTestOutbound) DialContext(ctx context.Context, _ string, _ M.Socksaddr) (net.Conn, error) {
	o.dials.Add(1)
	if o.dial != nil {
		return o.dial(ctx)
	}
	if o.fail.Load() {
		return nil, errors.New("dial failed")
	}
	client, peer := net.Pipe()
	go peer.Close()
	return client, nil
}

func (o *relayTestOutbound) ListenPacket(_ context.Context, destination M.Socksaddr) (net.PacketConn, error) {
	o.lastPacketDestination = destination
	return nil, errors.New("packet dial failed")
}

type relayTestEarlyConn struct {
	net.Conn
	handshakeErr error
	handshakes   atomic.Int32
}

var _ network.EarlyConn = (*relayTestEarlyConn)(nil)

func (c *relayTestEarlyConn) NeedHandshake() bool {
	return c.handshakes.Load() == 0
}

func (c *relayTestEarlyConn) Write(payload []byte) (int, error) {
	if c.NeedHandshake() {
		c.handshakes.Add(1)
		if c.handshakeErr != nil {
			return 0, c.handshakeErr
		}
	}
	if len(payload) == 0 {
		return 0, nil
	}
	return c.Conn.Write(payload)
}

func newRelayTestEarlyConn(handshakeErr error) *relayTestEarlyConn {
	client, peer := net.Pipe()
	go peer.Close()
	return &relayTestEarlyConn{Conn: client, handshakeErr: handshakeErr}
}

func newRelayCandidate(t *testing.T, server string, port int, tag string) (*node.NodeEntry, *relayTestOutbound) {
	t.Helper()
	raw, err := json.Marshal(map[string]any{
		"type":        "shadowsocks",
		"tag":         tag,
		"server":      server,
		"server_port": port,
	})
	if err != nil {
		t.Fatal(err)
	}
	hash := node.HashFromRawOptions(raw)
	entry := node.NewNodeEntry(hash, raw, time.Now(), 4)
	entry.AddSubscriptionID("direct-sub")
	entry.SetEgressIP(netip.MustParseAddr("203.0.113.10"))
	entry.LatencyTable.Update("example.com", 20*time.Millisecond, time.Minute)
	outbound := &relayTestOutbound{tag: tag, networks: []string{"tcp", "udp"}}
	var storeOutbound adapter.Outbound = outbound
	entry.Outbound.Store(&storeOutbound)
	return entry, outbound
}

func buildRelayTestPlatform(id string, entries map[node.Hash]*node.NodeEntry) *platform.Platform {
	value := platform.NewPlatform(id, "relay", nil, nil)
	value.FullRebuild(
		func(fn func(node.Hash, *node.NodeEntry) bool) {
			for hash, entry := range entries {
				if !fn(hash, entry) {
					return
				}
			}
		},
		func(string, node.Hash) (string, bool, []string, bool) {
			return "direct", true, []string{"relay"}, true
		},
		nil,
	)
	return value
}

func TestPlatformRelayDialer_MissingAndEmptyPlatform(t *testing.T) {
	pool := &relayTestPool{entries: map[node.Hash]*node.NodeEntry{}, platforms: map[string]*platform.Platform{}}
	resolver := func(*node.NodeEntry) (string, error) { return "", nil }
	dialer := newPlatformRelayDialer(pool, "missing", resolver, nil)
	_, err := dialer.DialContext(context.Background(), "tcp", M.ParseSocksaddr("198.51.100.10:443"))
	if !errors.Is(err, ErrRelayPlatformNotFound) {
		t.Fatalf("missing Platform error = %v", err)
	}

	pool.platforms["empty"] = buildRelayTestPlatform("empty", pool.entries)
	dialer = newPlatformRelayDialer(pool, "empty", resolver, nil)
	_, err = dialer.DialContext(context.Background(), "tcp", M.ParseSocksaddr("198.51.100.10:443"))
	if !errors.Is(err, ErrNoRelayCandidates) {
		t.Fatalf("empty Platform error = %v", err)
	}
}

func TestPlatformRelayDialer_RetriesCandidateAndSucceeds(t *testing.T) {
	entries := make(map[node.Hash]*node.NodeEntry)
	firstEntry, _ := newRelayCandidate(t, "203.0.113.1", 1001, "first")
	secondEntry, _ := newRelayCandidate(t, "203.0.113.2", 1002, "second")
	entries[firstEntry.Hash] = firstEntry
	entries[secondEntry.Hash] = secondEntry
	pool := &relayTestPool{
		entries:   entries,
		platforms: map[string]*platform.Platform{"relay-id": buildRelayTestPlatform("relay-id", entries)},
	}
	dialer := newPlatformRelayDialer(pool, "relay-id", func(*node.NodeEntry) (string, error) { return "", nil }, nil)
	candidates, err := dialer.snapshotCandidates("tcp", M.ParseSocksaddr("198.51.100.10:443"))
	if err != nil {
		t.Fatal(err)
	}
	first := candidates[0].outbound.(*relayTestOutbound)
	first.fail.Store(true)

	conn, err := dialer.DialContext(context.Background(), "tcp", M.ParseSocksaddr("198.51.100.10:443"))
	if err != nil {
		t.Fatalf("DialContext: %v", err)
	}
	_ = conn.Close()
	if first.dials.Load() != 1 {
		t.Fatalf("first candidate dials = %d, want 1", first.dials.Load())
	}
}

func TestPlatformRelayDialer_RetriesEarlyHandshakeFailureAndProbesFailedCandidate(t *testing.T) {
	entries := make(map[node.Hash]*node.NodeEntry)
	firstEntry, _ := newRelayCandidate(t, "203.0.113.1", 1001, "first")
	secondEntry, _ := newRelayCandidate(t, "203.0.113.2", 1002, "second")
	entries[firstEntry.Hash] = firstEntry
	entries[secondEntry.Hash] = secondEntry
	pool := &relayTestPool{
		entries:   entries,
		platforms: map[string]*platform.Platform{"relay-id": buildRelayTestPlatform("relay-id", entries)},
	}
	var failed []node.Hash
	dialer := newPlatformRelayDialer(
		pool,
		"relay-id",
		func(*node.NodeEntry) (string, error) { return "", nil },
		func(hash node.Hash) { failed = append(failed, hash) },
	)
	candidates, err := dialer.snapshotCandidates("tcp", M.ParseSocksaddr("198.51.100.10:443"))
	if err != nil {
		t.Fatal(err)
	}
	first := candidates[0]
	firstConn := newRelayTestEarlyConn(ws.StatusError(404))
	first.outbound.(*relayTestOutbound).dial = func(context.Context) (net.Conn, error) {
		return firstConn, nil
	}

	conn, err := dialer.DialContext(context.Background(), "tcp", M.ParseSocksaddr("198.51.100.10:443"))
	if err != nil {
		t.Fatalf("DialContext: %v", err)
	}
	_ = conn.Close()
	if firstConn.handshakes.Load() != 1 {
		t.Fatalf("first candidate handshakes = %d, want 1", firstConn.handshakes.Load())
	}
	if len(failed) != 1 || failed[0] != first.hash {
		t.Fatalf("failed candidate probes = %v, want [%s]", failed, first.hash.Hex())
	}
}

func TestPlatformRelayDialer_AllEarlyHandshakesFail(t *testing.T) {
	entries := make(map[node.Hash]*node.NodeEntry)
	for i := 0; i < maxRelayDialAttempts; i++ {
		entry, outbound := newRelayCandidate(t, netip.AddrFrom4([4]byte{203, 0, 113, byte(i + 1)}).String(), 3000+i, string(rune('a'+i)))
		outbound.dial = func(context.Context) (net.Conn, error) {
			return newRelayTestEarlyConn(ws.StatusError(404)), nil
		}
		entries[entry.Hash] = entry
	}
	pool := &relayTestPool{
		entries:   entries,
		platforms: map[string]*platform.Platform{"relay-id": buildRelayTestPlatform("relay-id", entries)},
	}
	dialer := newPlatformRelayDialer(pool, "relay-id", func(*node.NodeEntry) (string, error) { return "", nil }, nil)

	_, err := dialer.DialContext(context.Background(), "tcp", M.ParseSocksaddr("198.51.100.10:443"))
	var relayErr *RelayDialError
	if !errors.As(err, &relayErr) || relayErr.Attempts != maxRelayDialAttempts {
		t.Fatalf("relay error = %#v", err)
	}
	var statusErr ws.StatusError
	if !errors.As(err, &statusErr) || statusErr != ws.StatusError(404) {
		t.Fatalf("status error = %v, want 404", err)
	}
}

func TestPlatformRelayDialer_EarlyHandshakeSuccessReturnsOriginalConn(t *testing.T) {
	entry, outbound := newRelayCandidate(t, "203.0.113.1", 1001, "relay")
	earlyConn := newRelayTestEarlyConn(nil)
	outbound.dial = func(context.Context) (net.Conn, error) { return earlyConn, nil }
	entries := map[node.Hash]*node.NodeEntry{entry.Hash: entry}
	pool := &relayTestPool{
		entries:   entries,
		platforms: map[string]*platform.Platform{"relay-id": buildRelayTestPlatform("relay-id", entries)},
	}
	dialer := newPlatformRelayDialer(pool, "relay-id", func(*node.NodeEntry) (string, error) { return "", nil }, nil)

	conn, err := dialer.DialContext(context.Background(), "tcp", M.ParseSocksaddr("198.51.100.10:443"))
	if err != nil {
		t.Fatalf("DialContext: %v", err)
	}
	defer conn.Close()
	if conn != earlyConn {
		t.Fatalf("returned conn = %T %p, want original %p", conn, conn, earlyConn)
	}
	if earlyConn.handshakes.Load() != 1 {
		t.Fatalf("handshakes = %d, want 1", earlyConn.handshakes.Load())
	}
}

func TestPlatformRelayDialer_ContextCanceledStopsWithoutProbe(t *testing.T) {
	entries := make(map[node.Hash]*node.NodeEntry)
	for i := 0; i < 2; i++ {
		entry, outbound := newRelayCandidate(t, netip.AddrFrom4([4]byte{203, 0, 113, byte(i + 1)}).String(), 4000+i, string(rune('a'+i)))
		outbound.dial = func(context.Context) (net.Conn, error) { return nil, context.Canceled }
		entries[entry.Hash] = entry
	}
	pool := &relayTestPool{
		entries:   entries,
		platforms: map[string]*platform.Platform{"relay-id": buildRelayTestPlatform("relay-id", entries)},
	}
	probes := 0
	dialer := newPlatformRelayDialer(
		pool,
		"relay-id",
		func(*node.NodeEntry) (string, error) { return "", nil },
		func(node.Hash) { probes++ },
	)

	_, err := dialer.DialContext(context.Background(), "tcp", M.ParseSocksaddr("198.51.100.10:443"))
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("DialContext error = %v, want context canceled", err)
	}
	totalDials := int32(0)
	for _, entry := range entries {
		totalDials += (*entry.Outbound.Load()).(*relayTestOutbound).dials.Load()
	}
	if totalDials != 1 || probes != 0 {
		t.Fatalf("dials = %d, probes = %d; want 1, 0", totalDials, probes)
	}
}

func TestPlatformRelayDialer_BoundsAttemptsAndRejectsSecondHop(t *testing.T) {
	entries := make(map[node.Hash]*node.NodeEntry)
	for i := 0; i < 4; i++ {
		entry, outbound := newRelayCandidate(t, netip.AddrFrom4([4]byte{203, 0, 113, byte(i + 1)}).String(), 2000+i, string(rune('a'+i)))
		outbound.fail.Store(true)
		entries[entry.Hash] = entry
	}
	pool := &relayTestPool{
		entries:   entries,
		platforms: map[string]*platform.Platform{"relay-id": buildRelayTestPlatform("relay-id", entries)},
	}
	dialer := newPlatformRelayDialer(pool, "relay-id", func(*node.NodeEntry) (string, error) { return "", nil }, nil)
	_, err := dialer.DialContext(context.Background(), "tcp", M.ParseSocksaddr("198.51.100.10:443"))
	var relayErr *RelayDialError
	if !errors.As(err, &relayErr) || relayErr.Attempts != maxRelayDialAttempts {
		t.Fatalf("relay error = %#v", err)
	}
	totalDials := int32(0)
	for _, entry := range entries {
		totalDials += (*entry.Outbound.Load()).(*relayTestOutbound).dials.Load()
	}
	if totalDials != maxRelayDialAttempts {
		t.Fatalf("total candidate dials = %d, want %d", totalDials, maxRelayDialAttempts)
	}

	dialer = newPlatformRelayDialer(pool, "relay-id", func(*node.NodeEntry) (string, error) { return "another-relay", nil }, nil)
	_, err = dialer.DialContext(context.Background(), "tcp", M.ParseSocksaddr("198.51.100.10:443"))
	if !errors.Is(err, ErrNoRelayCandidates) {
		t.Fatalf("second-hop filter error = %v", err)
	}
}

func TestPlatformRelayDialer_RejectsTargetSelfProxy(t *testing.T) {
	entry, _ := newRelayCandidate(t, "198.51.100.10", 443, "self")
	entries := map[node.Hash]*node.NodeEntry{entry.Hash: entry}
	pool := &relayTestPool{
		entries:   entries,
		platforms: map[string]*platform.Platform{"relay-id": buildRelayTestPlatform("relay-id", entries)},
	}
	dialer := newPlatformRelayDialer(pool, "relay-id", func(*node.NodeEntry) (string, error) { return "", nil }, nil)
	_, err := dialer.DialContext(context.Background(), "tcp", M.ParseSocksaddr("198.51.100.10:443"))
	if !errors.Is(err, ErrNoRelayCandidates) {
		t.Fatalf("self-proxy filter error = %v", err)
	}
}

func TestInjectRelayPlatformDetour_ClonesAndRejectsConflict(t *testing.T) {
	raw := json.RawMessage(`{"type":"shadowsocks","tag":"target","server":"198.51.100.10","server_port":443}`)
	original := append(json.RawMessage(nil), raw...)
	patched, err := injectRelayPlatformDetour(raw, "resin-relay-platform-test")
	if err != nil {
		t.Fatalf("injectRelayPlatformDetour: %v", err)
	}
	if string(raw) != string(original) {
		t.Fatal("persisted raw options were mutated")
	}
	var decoded map[string]any
	if err := json.Unmarshal(patched, &decoded); err != nil {
		t.Fatal(err)
	}
	if decoded["detour"] != "resin-relay-platform-test" {
		t.Fatalf("patched detour = %v", decoded["detour"])
	}

	_, err = injectRelayPlatformDetour(
		json.RawMessage(`{"type":"shadowsocks","detour":"existing"}`),
		"resin-relay-platform-test",
	)
	if err == nil {
		t.Fatal("existing node detour should conflict with subscription relay Platform")
	}
}

func TestMihomoRelayDialer_ListenPacketUsesRemoteDestination(t *testing.T) {
	entry, candidate := newRelayCandidate(t, "203.0.113.1", 1001, "relay")
	entries := map[node.Hash]*node.NodeEntry{entry.Hash: entry}
	pool := &relayTestPool{
		entries:   entries,
		platforms: map[string]*platform.Platform{"relay-id": buildRelayTestPlatform("relay-id", entries)},
	}
	dialer := &mihomoRelayDialer{dialer: newPlatformRelayDialer(
		pool,
		"relay-id",
		func(*node.NodeEntry) (string, error) { return "", nil },
		nil,
	)}
	remote := netip.MustParseAddrPort("198.51.100.53:53")
	_, err := dialer.ListenPacket(context.Background(), "udp", "0.0.0.0:0", remote)
	if err == nil {
		t.Fatal("packet relay should expose the candidate test error")
	}
	if candidate.lastPacketDestination != M.SocksaddrFromNetIP(remote) {
		t.Fatalf("packet destination = %v, want %v", candidate.lastPacketDestination, remote)
	}
}

func TestPlatformRelayDialer_UDPEagerFailureTriggersProbe(t *testing.T) {
	entry, _ := newRelayCandidate(t, "203.0.113.1", 1001, "relay")
	entries := map[node.Hash]*node.NodeEntry{entry.Hash: entry}
	pool := &relayTestPool{
		entries:   entries,
		platforms: map[string]*platform.Platform{"relay-id": buildRelayTestPlatform("relay-id", entries)},
	}
	var failed []node.Hash
	dialer := newPlatformRelayDialer(
		pool,
		"relay-id",
		func(*node.NodeEntry) (string, error) { return "", nil },
		func(hash node.Hash) { failed = append(failed, hash) },
	)

	_, err := dialer.ListenPacket(context.Background(), M.ParseSocksaddr("198.51.100.53:53"))
	if err == nil {
		t.Fatal("ListenPacket should expose the candidate error")
	}
	if len(failed) != 1 || failed[0] != entry.Hash {
		t.Fatalf("failed candidate probes = %v, want [%s]", failed, entry.Hash.Hex())
	}
}

func TestSingboxBuilder_BuildsAndReusesRelayPlatformDetour(t *testing.T) {
	const relayPlatformID = "11111111-1111-1111-1111-111111111111"
	pool := &relayTestPool{entries: map[node.Hash]*node.NodeEntry{}, platforms: map[string]*platform.Platform{}}
	pool.platforms[relayPlatformID] = buildRelayTestPlatform(relayPlatformID, pool.entries)
	builder, err := NewSingboxBuilderWithConfig(SingboxBuilderConfig{
		DNSUpstreams: config.DefaultNodeDNSUpstreams(),
		RelayPool:    pool,
		ResolveRelayPlatformID: func(*node.NodeEntry) (string, error) {
			return "", nil
		},
	})
	if err != nil {
		t.Fatalf("NewSingboxBuilderWithConfig: %v", err)
	}
	defer builder.Close()

	raw := json.RawMessage(`{
		"type":"socks",
		"tag":"relay-target",
		"server":"127.0.0.1",
		"server_port":1080
	}`)
	first, err := builder.Build(raw, relayPlatformID)
	if err != nil {
		t.Fatalf("first relayed Build: %v", err)
	}
	defer closeOutbound(first)
	detourTag := "resin-relay-platform-" + relayPlatformID
	group, ok := builder.outboundManager.Outbound(detourTag)
	if !ok || group == nil {
		t.Fatal("relay Platform detour was not registered")
	}

	second, err := builder.Build(raw, relayPlatformID)
	if err != nil {
		t.Fatalf("second relayed Build: %v", err)
	}
	defer closeOutbound(second)
	reused, ok := builder.outboundManager.Outbound(detourTag)
	if !ok || reused != group {
		t.Fatal("relay Platform detour should be reused")
	}
}

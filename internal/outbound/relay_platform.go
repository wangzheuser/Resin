package outbound

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"net/netip"
	"sort"
	"strings"
	"sync/atomic"

	"github.com/Resinat/Resin/internal/node"
	"github.com/Resinat/Resin/internal/platform"
	"github.com/sagernet/sing-box/adapter"
	M "github.com/sagernet/sing/common/metadata"
)

const (
	relayPlatformOutboundType = "resin-relay-platform"
	maxRelayDialAttempts      = 3
)

var (
	// ErrRelayPlatformNotFound indicates that the referenced Platform is absent.
	ErrRelayPlatformNotFound = errors.New("relay platform not found")
	// ErrNoRelayCandidates indicates that the Platform has no eligible direct node.
	ErrNoRelayCandidates = errors.New("no eligible relay candidates")
)

// NodeRelayPlatformResolver resolves the relay Platform assigned to a node.
// Empty output means the node is direct.
type NodeRelayPlatformResolver func(entry *node.NodeEntry) (string, error)

// RelayPoolAccessor provides the node and Platform reads required by the
// single-hop relay selector.
type RelayPoolAccessor interface {
	GetEntry(hash node.Hash) (*node.NodeEntry, bool)
	GetPlatform(id string) (*platform.Platform, bool)
}

// RelayDialError reports a failed bounded relay attempt sequence.
type RelayDialError struct {
	PlatformID string
	Attempts   int
	Err        error
}

// Error implements error.
func (e *RelayDialError) Error() string {
	return fmt.Sprintf("relay platform %s: %d dial attempt(s) failed: %v", e.PlatformID, e.Attempts, e.Err)
}

// Unwrap exposes the joined candidate errors.
func (e *RelayDialError) Unwrap() error { return e.Err }

type relayPlatformOptions struct {
	PlatformID string `json:"platform_id"`
}

type relayCandidate struct {
	hash     node.Hash
	outbound adapter.Outbound
}

// platformRelayDialer dynamically selects a healthy direct node from one
// Platform for every target-server connection.
type platformRelayDialer struct {
	pool                   RelayPoolAccessor
	platformID             string
	resolveRelayPlatformID NodeRelayPlatformResolver
	cursor                 atomic.Uint64
}

// newPlatformRelayDialer creates a dynamic, fail-closed single-hop selector.
func newPlatformRelayDialer(
	pool RelayPoolAccessor,
	platformID string,
	resolveRelayPlatformID NodeRelayPlatformResolver,
) *platformRelayDialer {
	return &platformRelayDialer{
		pool:                   pool,
		platformID:             platformID,
		resolveRelayPlatformID: resolveRelayPlatformID,
	}
}

// DialContext opens a TCP-like connection to destination through an eligible
// node in the configured Platform.
func (d *platformRelayDialer) DialContext(
	ctx context.Context,
	network string,
	destination M.Socksaddr,
) (net.Conn, error) {
	candidates, err := d.snapshotCandidates(network, destination)
	if err != nil {
		return nil, err
	}

	attempts := min(maxRelayDialAttempts, len(candidates))
	start := int(d.cursor.Add(1)-1) % len(candidates)
	var dialErrors []error
	for i := 0; i < attempts; i++ {
		candidate := candidates[(start+i)%len(candidates)]
		conn, dialErr := candidate.outbound.DialContext(ctx, network, destination)
		if dialErr == nil {
			return conn, nil
		}
		dialErrors = append(dialErrors, fmt.Errorf("candidate %s: %w", candidate.hash.Hex(), dialErr))
	}
	return nil, &RelayDialError{
		PlatformID: d.platformID,
		Attempts:   attempts,
		Err:        errors.Join(dialErrors...),
	}
}

// ListenPacket opens a packet connection through an eligible UDP-capable node.
func (d *platformRelayDialer) ListenPacket(
	ctx context.Context,
	destination M.Socksaddr,
) (net.PacketConn, error) {
	candidates, err := d.snapshotCandidates("udp", destination)
	if err != nil {
		return nil, err
	}

	attempts := min(maxRelayDialAttempts, len(candidates))
	start := int(d.cursor.Add(1)-1) % len(candidates)
	var dialErrors []error
	for i := 0; i < attempts; i++ {
		candidate := candidates[(start+i)%len(candidates)]
		packetConn, dialErr := candidate.outbound.ListenPacket(ctx, destination)
		if dialErr == nil {
			return packetConn, nil
		}
		dialErrors = append(dialErrors, fmt.Errorf("candidate %s: %w", candidate.hash.Hex(), dialErr))
	}
	return nil, &RelayDialError{
		PlatformID: d.platformID,
		Attempts:   attempts,
		Err:        errors.Join(dialErrors...),
	}
}

// snapshotCandidates copies the Platform view before inspecting or dialing so
// network operations never run while a RoutableView shard lock is held.
func (d *platformRelayDialer) snapshotCandidates(
	network string,
	destination M.Socksaddr,
) ([]relayCandidate, error) {
	if d == nil || d.pool == nil {
		return nil, ErrRelayPlatformNotFound
	}
	relayPlatform, ok := d.pool.GetPlatform(d.platformID)
	if !ok {
		return nil, fmt.Errorf("%w: %s", ErrRelayPlatformNotFound, d.platformID)
	}

	hashes := make([]node.Hash, 0, relayPlatform.View().Size())
	relayPlatform.View().Range(func(hash node.Hash) bool {
		hashes = append(hashes, hash)
		return true
	})
	// RoutableView is map-backed, so normalize its snapshot order before
	// applying the shared cursor. This makes round-robin selection stable.
	sort.Slice(hashes, func(i, j int) bool {
		return bytes.Compare(hashes[i][:], hashes[j][:]) < 0
	})

	candidates := make([]relayCandidate, 0, len(hashes))
	for _, hash := range hashes {
		entry, exists := d.pool.GetEntry(hash)
		if !exists || entry == nil || rawOutboundHasDetour(entry.RawOptions) {
			continue
		}
		if d.resolveRelayPlatformID == nil {
			continue
		}
		candidateRelayPlatformID, resolveErr := d.resolveRelayPlatformID(entry)
		if resolveErr != nil || candidateRelayPlatformID != "" {
			// A relayed candidate would create a second hop.
			continue
		}
		if rawOutboundTargetsDestination(entry.RawOptions, destination) {
			// Avoid asking the target node to proxy a connection to itself.
			continue
		}
		outboundPtr := entry.Outbound.Load()
		if outboundPtr == nil || !outboundSupportsNetwork(*outboundPtr, network) {
			continue
		}
		candidates = append(candidates, relayCandidate{hash: hash, outbound: *outboundPtr})
	}
	if len(candidates) == 0 {
		return nil, fmt.Errorf("%w: platform %s", ErrNoRelayCandidates, d.platformID)
	}
	return candidates, nil
}

// rawOutboundHasDetour reports whether the node already depends on another
// outbound and therefore is ineligible as a direct relay candidate.
func rawOutboundHasDetour(raw json.RawMessage) bool {
	var header struct {
		Detour string `json:"detour"`
	}
	return json.Unmarshal(raw, &header) == nil && strings.TrimSpace(header.Detour) != ""
}

// rawOutboundTargetsDestination detects the target-server self-proxy case.
func rawOutboundTargetsDestination(raw json.RawMessage, destination M.Socksaddr) bool {
	var header struct {
		Server     string `json:"server"`
		ServerPort uint16 `json:"server_port"`
	}
	if json.Unmarshal(raw, &header) != nil || header.Server == "" || header.ServerPort != destination.Port {
		return false
	}
	if destination.IsFqdn() {
		return normalizedHostname(header.Server) == normalizedHostname(destination.Fqdn)
	}
	serverAddr, err := netip.ParseAddr(strings.Trim(header.Server, "[]"))
	return err == nil && serverAddr.Unmap() == destination.Addr.Unmap()
}

// normalizedHostname canonicalizes DNS names for self-target comparison.
func normalizedHostname(value string) string {
	return strings.TrimSuffix(strings.ToLower(strings.TrimSpace(value)), ".")
}

// outboundSupportsNetwork checks the candidate's advertised network families.
func outboundSupportsNetwork(outbound adapter.Outbound, network string) bool {
	wanted := "tcp"
	if strings.HasPrefix(strings.ToLower(network), "udp") {
		wanted = "udp"
	}
	networks := outbound.Network()
	if len(networks) == 0 {
		return true
	}
	for _, supported := range networks {
		if strings.EqualFold(supported, wanted) {
			return true
		}
	}
	return false
}

type relayPlatformOutbound struct {
	tag    string
	dialer *platformRelayDialer
}

// newRelayPlatformOutbound creates the internal sing-box detour endpoint.
func newRelayPlatformOutbound(tag string, dialer *platformRelayDialer) adapter.Outbound {
	return &relayPlatformOutbound{tag: tag, dialer: dialer}
}

// Type returns the private internal outbound type.
func (o *relayPlatformOutbound) Type() string { return relayPlatformOutboundType }

// Tag returns the detour lookup tag.
func (o *relayPlatformOutbound) Tag() string { return o.tag }

// Network advertises TCP and UDP candidate selection.
func (o *relayPlatformOutbound) Network() []string { return []string{"tcp", "udp"} }

// Dependencies reports no sing-box outbound dependencies.
func (o *relayPlatformOutbound) Dependencies() []string { return nil }

// DialContext delegates target-server dialing to the dynamic Platform selector.
func (o *relayPlatformOutbound) DialContext(ctx context.Context, network string, destination M.Socksaddr) (net.Conn, error) {
	return o.dialer.DialContext(ctx, network, destination)
}

// ListenPacket delegates target-server packet dialing to the Platform selector.
func (o *relayPlatformOutbound) ListenPacket(ctx context.Context, destination M.Socksaddr) (net.PacketConn, error) {
	return o.dialer.ListenPacket(ctx, destination)
}

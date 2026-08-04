package outbound

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"sync"

	"github.com/Resinat/Resin/internal/node"
	"github.com/sagernet/sing-box/adapter"
	"github.com/sagernet/sing-box/adapter/endpoint"
	"github.com/sagernet/sing-box/adapter/inbound"
	sbOutbound "github.com/sagernet/sing-box/adapter/outbound"
	"github.com/sagernet/sing-box/dns"
	"github.com/sagernet/sing-box/include"
	"github.com/sagernet/sing-box/log"
	"github.com/sagernet/sing-box/option"
	"github.com/sagernet/sing/common"
	sJson "github.com/sagernet/sing/common/json"
	"github.com/sagernet/sing/service"
)

// OutboundBuilder creates outbound instances from raw node options.
type OutboundBuilder interface {
	Build(rawOptions json.RawMessage, relayPlatformID ...string) (adapter.Outbound, error)
}

// SingboxBuilderConfig configures SingboxBuilder construction.
type SingboxBuilderConfig struct {
	// DNSUpstreams configures Resin's node DNS chain.
	// Values are DNS upstream URI strings and the slice must not be empty.
	DNSUpstreams []string
	// RelayPool and ResolveRelayPlatformID enable subscription-level single-hop
	// relay Platforms. Direct-only callers may leave both unset.
	RelayPool               RelayPoolAccessor
	ResolveRelayPlatformID  NodeRelayPlatformResolver
	OnRelayCandidateFailure func(node.Hash)
}

// ---------------------------------------------------------------------------
// SingboxBuilder — creates real sing-box adapter.Outbound instances.
// ---------------------------------------------------------------------------

// SingboxBuilder builds real sing-box outbound instances from raw JSON options.
// It holds a fully-wired context with DNS services so that domain-based
// outbound servers can be resolved.
type SingboxBuilder struct {
	registry                *sbOutbound.Registry
	ctx                     context.Context
	logFactory              log.Factory
	dnsTransportManager     *dns.TransportManager
	dnsRouter               *dns.Router
	outboundManager         *sbOutbound.Manager
	relayPool               RelayPoolAccessor
	resolveRelayPlatformID  NodeRelayPlatformResolver
	onRelayCandidateFailure func(node.Hash)
	relayMu                 sync.Mutex
}

// NewSingboxBuilderWithConfig creates a SingboxBuilder with a complete
// sing-box service graph (registries + DNS). The caller must call Close()
// when done.
func NewSingboxBuilderWithConfig(cfg SingboxBuilderConfig) (*SingboxBuilder, error) {
	ctx := context.Background()
	ctx = include.Context(ctx) // inject protocol registries

	logFactory := log.NewNOPFactory()
	logger := logFactory.NewLogger("resin-outbound")

	dnsRegistry, ok := service.FromContext[adapter.DNSTransportRegistry](ctx).(*dns.TransportRegistry)
	if !ok {
		return nil, fmt.Errorf("singbox builder: unexpected DNS transport registry type %T", service.FromContext[adapter.DNSTransportRegistry](ctx))
	}
	registerSecureDNSTransport(dnsRegistry)

	// --- Service graph (same order as Demos/simple-proxy/main.go) -----------

	// Endpoint Manager
	endpointMgr := endpoint.NewManager(logger, service.FromContext[adapter.EndpointRegistry](ctx))
	service.MustRegister[adapter.EndpointManager](ctx, endpointMgr)

	// Inbound Manager (required dependency even though unused)
	inboundMgr := inbound.NewManager(logger, service.FromContext[adapter.InboundRegistry](ctx), endpointMgr)
	service.MustRegister[adapter.InboundManager](ctx, inboundMgr)

	// Outbound Manager (sing-box's own manager, for detour resolution)
	outboundMgr := sbOutbound.NewManager(logger, service.FromContext[adapter.OutboundRegistry](ctx), endpointMgr, "")
	service.MustRegister[adapter.OutboundManager](ctx, outboundMgr)

	// DNS Transport Manager
	dnsTransportMgr := dns.NewTransportManager(logger, service.FromContext[adapter.DNSTransportRegistry](ctx), outboundMgr, secureDNSFailoverTransportTag)
	service.MustRegister[adapter.DNSTransportManager](ctx, dnsTransportMgr)

	// DNS Router
	dnsRouter := dns.NewRouter(ctx, logFactory, option.DNSOptions{})
	service.MustRegister[adapter.DNSRouter](ctx, dnsRouter)

	dnsTransportSpecs, err := secureDNSTransportSpecsForUpstreams(cfg.DNSUpstreams)
	if err != nil {
		return nil, fmt.Errorf("singbox builder: configure DNS transports: %w", err)
	}
	for _, spec := range dnsTransportSpecs {
		if err := dnsTransportMgr.Create(ctx, logger, spec.tag, spec.transportType, spec.options); err != nil {
			return nil, fmt.Errorf("singbox builder: create DNS transport %s[%s]: %w", spec.transportType, spec.tag, err)
		}
	}

	// Start DNS Transport Manager lifecycle
	if err := dnsTransportMgr.Start(adapter.StartStateInitialize); err != nil {
		return nil, fmt.Errorf("singbox builder: initialize DNS transport manager: %w", err)
	}
	if err := dnsTransportMgr.Start(adapter.StartStateStart); err != nil {
		_ = dnsTransportMgr.Close()
		return nil, fmt.Errorf("singbox builder: start DNS transport manager: %w", err)
	}

	// Start DNS Router lifecycle
	if err := dnsRouter.Initialize(nil); err != nil {
		_ = dnsTransportMgr.Close()
		return nil, fmt.Errorf("singbox builder: initialize DNS router: %w", err)
	}
	if err := dnsRouter.Start(adapter.StartStateStart); err != nil {
		_ = dnsRouter.Close()
		_ = dnsTransportMgr.Close()
		return nil, fmt.Errorf("singbox builder: start DNS router: %w", err)
	}

	registry := service.FromContext[adapter.OutboundRegistry](ctx).(*sbOutbound.Registry)
	sbOutbound.Register[relayPlatformOptions](registry, relayPlatformOutboundType,
		func(_ context.Context, _ adapter.Router, _ log.ContextLogger, tag string, options relayPlatformOptions) (adapter.Outbound, error) {
			if strings.TrimSpace(options.PlatformID) == "" {
				return nil, fmt.Errorf("relay platform id is required")
			}
			dialer := newPlatformRelayDialer(cfg.RelayPool, options.PlatformID, cfg.ResolveRelayPlatformID, cfg.OnRelayCandidateFailure)
			return newRelayPlatformOutbound(tag, dialer), nil
		})

	return &SingboxBuilder{
		registry:                registry,
		ctx:                     ctx,
		logFactory:              logFactory,
		dnsTransportManager:     dnsTransportMgr,
		dnsRouter:               dnsRouter,
		outboundManager:         outboundMgr,
		relayPool:               cfg.RelayPool,
		resolveRelayPlatformID:  cfg.ResolveRelayPlatformID,
		onRelayCandidateFailure: cfg.OnRelayCandidateFailure,
	}, nil
}

// Build parses rawOptions (a complete sing-box outbound JSON object with
// type/tag fields) into a real adapter.Outbound and runs it through the
// lifecycle stages.
func (b *SingboxBuilder) Build(rawOptions json.RawMessage, relayPlatformIDs ...string) (adapter.Outbound, error) {
	relayPlatformID := firstRelayPlatformID(relayPlatformIDs)
	if relayPlatformID != "" {
		detourTag, err := b.ensureRelayPlatformOutbound(relayPlatformID)
		if err != nil {
			return nil, err
		}
		rawOptions, err = injectRelayPlatformDetour(rawOptions, detourTag)
		if err != nil {
			return nil, err
		}
	}

	// 1. Parse via official option.Outbound path (strips type/tag, creates
	//    typed options via OutboundOptionsRegistry + badjson.UnmarshallExcluded).
	var outboundConfig option.Outbound
	if err := sJson.UnmarshalContext(b.ctx, rawOptions, &outboundConfig); err != nil {
		return nil, fmt.Errorf("parse outbound options: %w", err)
	}

	// 2. Create the outbound instance via the registry.
	logger := b.logFactory.NewLogger("outbound/" + outboundConfig.Type)
	ob, err := b.registry.CreateOutbound(
		b.ctx,
		nil, // router — not needed for simple dialing
		logger,
		outboundConfig.Tag,
		outboundConfig.Type,
		outboundConfig.Options,
	)
	if err != nil {
		return nil, fmt.Errorf("create outbound [%s]: %w", outboundConfig.Type, err)
	}

	// 3. Run lifecycle start stages. On failure, close and return error.
	for _, stage := range adapter.ListStartStages {
		if err := adapter.LegacyStart(ob, stage); err != nil {
			_ = common.Close(ob)
			return nil, fmt.Errorf("outbound start %s [%s]: %w", stage, outboundConfig.Type, err)
		}
	}

	return ob, nil
}

// RelayDialer creates the same dynamic Platform selector used by sing-box
// detours so alternative protocol builders can share relay semantics.
func (b *SingboxBuilder) RelayDialer(platformID string) (*platformRelayDialer, error) {
	if b == nil || b.relayPool == nil {
		return nil, fmt.Errorf("relay platform runtime is not configured")
	}
	if _, ok := b.relayPool.GetPlatform(platformID); !ok {
		return nil, fmt.Errorf("%w: %s", ErrRelayPlatformNotFound, platformID)
	}
	return newPlatformRelayDialer(b.relayPool, platformID, b.resolveRelayPlatformID, b.onRelayCandidateFailure), nil
}

// ensureRelayPlatformOutbound registers one shared dynamic detour per Platform.
func (b *SingboxBuilder) ensureRelayPlatformOutbound(platformID string) (string, error) {
	detourTag := "resin-relay-platform-" + platformID
	b.relayMu.Lock()
	defer b.relayMu.Unlock()
	if _, ok := b.outboundManager.Outbound(detourTag); ok {
		return detourTag, nil
	}
	if b.relayPool == nil {
		return "", fmt.Errorf("relay platform runtime is not configured")
	}
	if _, ok := b.relayPool.GetPlatform(platformID); !ok {
		return "", fmt.Errorf("%w: %s", ErrRelayPlatformNotFound, platformID)
	}
	logger := b.logFactory.NewLogger("outbound/" + relayPlatformOutboundType)
	if err := b.outboundManager.Create(
		b.ctx,
		nil,
		logger,
		detourTag,
		relayPlatformOutboundType,
		&relayPlatformOptions{PlatformID: platformID},
	); err != nil {
		return "", fmt.Errorf("create relay platform detour: %w", err)
	}
	return detourTag, nil
}

// firstRelayPlatformID reads the optional single-hop builder argument.
func firstRelayPlatformID(values []string) string {
	if len(values) == 0 {
		return ""
	}
	return strings.TrimSpace(values[0])
}

// injectRelayPlatformDetour clones the node JSON and adds Resin's internal
// detour without mutating the persisted raw options.
func injectRelayPlatformDetour(rawOptions json.RawMessage, detourTag string) (json.RawMessage, error) {
	var options map[string]any
	if err := json.Unmarshal(rawOptions, &options); err != nil {
		return nil, fmt.Errorf("parse relay target options: %w", err)
	}
	if existing, ok := options["detour"]; ok && strings.TrimSpace(fmt.Sprint(existing)) != "" {
		return nil, fmt.Errorf("node detour conflicts with subscription relay platform")
	}
	options["detour"] = detourTag
	patched, err := json.Marshal(options)
	if err != nil {
		return nil, fmt.Errorf("encode relay target options: %w", err)
	}
	return patched, nil
}

// Close shuts down the builder's internal DNS services.
func (b *SingboxBuilder) Close() error {
	var errs []error
	if b.dnsRouter != nil {
		errs = append(errs, b.dnsRouter.Close())
	}
	if b.dnsTransportManager != nil {
		errs = append(errs, b.dnsTransportManager.Close())
	}
	return errors.Join(errs...)
}

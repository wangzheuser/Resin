package proxy

import (
	"context"
	"crypto/rand"
	"errors"
	"fmt"
	"log"
	"math/big"
	"net"
	"net/http"
	"net/netip"
	"sort"
	"strconv"
	"sync"
	"time"

	"github.com/google/uuid"

	"github.com/Resinat/Resin/internal/model"
	"github.com/Resinat/Resin/internal/node"
	"github.com/Resinat/Resin/internal/outbound"
	"github.com/Resinat/Resin/internal/platform"
	"github.com/Resinat/Resin/internal/routing"
)

const (
	DefaultProxySessionMinPort     = 20000
	DefaultProxySessionMaxPort     = 40000
	DefaultProxySessionMaxActive   = 1000
	DefaultProxySessionPortRetries = 64
)

var (
	ErrProxySessionPlatformNotFound = errors.New("proxy session platform not found")
	ErrProxySessionNoAvailableNodes = errors.New("proxy session no available nodes")
	ErrProxySessionLimitExceeded    = errors.New("proxy session limit exceeded")
	ErrProxySessionPortUnavailable  = errors.New("proxy session port unavailable")
	ErrProxySessionNotFound         = errors.New("proxy session not found")
)

// ProxySessionPool provides the platform and node reads needed by session allocation.
type ProxySessionPool interface {
	outbound.PoolAccessor
	GetPlatformByName(name string) (*platform.Platform, bool)
}

// ProxySessionManagerConfig contains dependencies for browser proxy sessions.
type ProxySessionManagerConfig struct {
	Router            *routing.Router
	Pool              ProxySessionPool
	Health            HealthRecorder
	Events            EventEmitter
	MetricsSink       MetricsEventSink
	OutboundTransport OutboundTransportConfig
	TransportPool     *OutboundTransportPool
	ProxyBypassRules  []string
	BindHost          string
	MinPort           int
	MaxPort           int
	MaxActiveSessions int
	PortRetries       int
}

// ProxySessionCreateRequest describes a browser proxy session allocation.
type ProxySessionCreateRequest struct {
	PlatformName string
	TTL          time.Duration
}

// ProxySessionInfo describes an active browser proxy session.
type ProxySessionInfo struct {
	ID           string
	PlatformID   string
	PlatformName string
	Account      string
	ProxyURL     string
	NodeHash     string
	EgressIP     string
	TTL          time.Duration
	ExpiresAt    time.Time
}

type proxySession struct {
	info     ProxySessionInfo
	listener net.Listener
	server   *http.Server
	timer    *time.Timer
}

// ProxySessionManager owns temporary unauthenticated local proxy listeners.
type ProxySessionManager struct {
	cfg       ProxySessionManagerConfig
	mu        sync.Mutex
	sessions  map[string]*proxySession
	cursors   map[string]int
	closed    bool
	closeOnce sync.Once
}

// NewProxySessionManager creates a manager for browser proxy sessions.
func NewProxySessionManager(cfg ProxySessionManagerConfig) *ProxySessionManager {
	if cfg.BindHost == "" {
		cfg.BindHost = "127.0.0.1"
	}
	if cfg.MinPort == 0 {
		cfg.MinPort = DefaultProxySessionMinPort
	}
	if cfg.MaxPort == 0 {
		cfg.MaxPort = DefaultProxySessionMaxPort
	}
	if cfg.MaxActiveSessions == 0 {
		cfg.MaxActiveSessions = DefaultProxySessionMaxActive
	}
	if cfg.PortRetries == 0 {
		cfg.PortRetries = DefaultProxySessionPortRetries
	}
	if cfg.TransportPool == nil {
		cfg.TransportPool = NewOutboundTransportPool(normalizeOutboundTransportConfig(cfg.OutboundTransport))
	}
	return &ProxySessionManager{
		cfg:      cfg,
		sessions: make(map[string]*proxySession),
		cursors:  make(map[string]int),
	}
}

// Create allocates a new browser proxy session and starts its local listener.
func (m *ProxySessionManager) Create(req ProxySessionCreateRequest) (ProxySessionInfo, error) {
	if m == nil || m.cfg.Pool == nil || m.cfg.Router == nil {
		return ProxySessionInfo{}, fmt.Errorf("proxy session manager is not initialized")
	}

	plat, ok := m.cfg.Pool.GetPlatformByName(req.PlatformName)
	if !ok || plat == nil {
		return ProxySessionInfo{}, ErrProxySessionPlatformNotFound
	}
	ttl := req.TTL
	if ttl == 0 {
		ttl = time.Duration(plat.StickyTTLNs)
	}
	if ttl <= 0 {
		return ProxySessionInfo{}, fmt.Errorf("ttl must be positive")
	}

	nodeHash, egressIP, err := m.nextNode(plat)
	if err != nil {
		return ProxySessionInfo{}, err
	}

	m.mu.Lock()
	if m.closed {
		m.mu.Unlock()
		return ProxySessionInfo{}, http.ErrServerClosed
	}
	if len(m.sessions) >= m.cfg.MaxActiveSessions {
		m.mu.Unlock()
		return ProxySessionInfo{}, ErrProxySessionLimitExceeded
	}
	m.mu.Unlock()

	account := uuid.NewString()
	id := uuid.NewString()
	now := time.Now().UTC()
	expiresAt := now.Add(ttl)
	lease := model.Lease{
		PlatformID:     plat.ID,
		Account:        account,
		NodeHash:       nodeHash.Hex(),
		EgressIP:       egressIP.String(),
		CreatedAtNs:    now.UnixNano(),
		ExpiryNs:       expiresAt.UnixNano(),
		LastAccessedNs: now.UnixNano(),
	}
	if err := m.cfg.Router.UpsertLease(lease); err != nil {
		return ProxySessionInfo{}, fmt.Errorf("upsert proxy session lease: %w", err)
	}

	listener, port, err := m.listenRandomPort()
	if err != nil {
		m.cfg.Router.DeleteLease(plat.ID, account)
		return ProxySessionInfo{}, err
	}

	handler := NewForwardProxy(ForwardProxyConfig{
		FixedPlatformName: plat.Name,
		FixedAccount:      account,
		Router:            m.cfg.Router,
		Pool:              m.cfg.Pool,
		Health:            m.cfg.Health,
		Events:            m.cfg.Events,
		MetricsSink:       m.cfg.MetricsSink,
		OutboundTransport: m.cfg.OutboundTransport,
		TransportPool:     m.cfg.TransportPool,
		ProxyBypassRules:  m.cfg.ProxyBypassRules,
	})
	server := &http.Server{Handler: handler}
	info := ProxySessionInfo{
		ID:           id,
		PlatformID:   plat.ID,
		PlatformName: plat.Name,
		Account:      account,
		ProxyURL:     "http://" + net.JoinHostPort(m.cfg.BindHost, strconv.Itoa(port)),
		NodeHash:     nodeHash.Hex(),
		EgressIP:     egressIP.String(),
		TTL:          ttl,
		ExpiresAt:    expiresAt,
	}

	sess := &proxySession{
		info:     info,
		listener: listener,
		server:   server,
	}

	m.mu.Lock()
	if m.closed {
		m.mu.Unlock()
		sess.stopTimer()
		_ = listener.Close()
		m.cfg.Router.DeleteLease(plat.ID, account)
		return ProxySessionInfo{}, http.ErrServerClosed
	}
	if len(m.sessions) >= m.cfg.MaxActiveSessions {
		m.mu.Unlock()
		sess.stopTimer()
		_ = listener.Close()
		m.cfg.Router.DeleteLease(plat.ID, account)
		return ProxySessionInfo{}, ErrProxySessionLimitExceeded
	}
	// 在发布 session 前启动 TTL 回调；回调会等待当前锁释放，避免发布与清理之间出现空窗。
	sess.timer = time.AfterFunc(ttl, func() {
		if err := m.Release(info.PlatformName, info.ID); err != nil && !errors.Is(err, ErrProxySessionNotFound) {
			log.Printf("[proxy-session] ttl release %s: %v", info.ID, err)
		}
	})
	m.sessions[id] = sess
	m.mu.Unlock()

	go func() {
		if err := server.Serve(listener); err != nil && !errors.Is(err, http.ErrServerClosed) && !errors.Is(err, net.ErrClosed) {
			log.Printf("[proxy-session] serve %s: %v", id, err)
		}
	}()

	return info, nil
}

// Release closes a browser proxy session and removes its sticky lease.
func (m *ProxySessionManager) Release(platformName, id string) error {
	if m == nil {
		return ErrProxySessionNotFound
	}

	m.mu.Lock()
	sess, ok := m.sessions[id]
	if !ok {
		m.mu.Unlock()
		return ErrProxySessionNotFound
	}
	if platformName != "" && sess.info.PlatformName != platformName {
		m.mu.Unlock()
		return ErrProxySessionNotFound
	}
	delete(m.sessions, id)
	m.mu.Unlock()

	sess.stopTimer()
	if sess.server != nil {
		_ = sess.server.Close()
	} else if sess.listener != nil {
		_ = sess.listener.Close()
	}
	m.cfg.Router.DeleteLease(sess.info.PlatformID, sess.info.Account)
	return nil
}

// Close releases every active browser proxy session.
func (m *ProxySessionManager) Close() {
	if m == nil {
		return
	}
	m.closeOnce.Do(func() {
		m.mu.Lock()
		m.closed = true
		ids := make([]string, 0, len(m.sessions))
		for id := range m.sessions {
			ids = append(ids, id)
		}
		m.mu.Unlock()

		for _, id := range ids {
			_ = m.Release("", id)
		}
	})
}

func (s *proxySession) stopTimer() {
	if s != nil && s.timer != nil {
		s.timer.Stop()
	}
}

func (m *ProxySessionManager) nextNode(plat *platform.Platform) (node.Hash, netip.Addr, error) {
	candidates := m.routableCandidates(plat)
	if len(candidates) == 0 {
		return node.Zero, netip.Addr{}, ErrProxySessionNoAvailableNodes
	}

	m.mu.Lock()
	cursor := m.cursors[plat.ID]
	pick := candidates[cursor%len(candidates)]
	m.cursors[plat.ID] = cursor + 1
	m.mu.Unlock()

	return pick.hash, pick.egressIP, nil
}

type proxySessionCandidate struct {
	hash     node.Hash
	egressIP netip.Addr
}

func (m *ProxySessionManager) routableCandidates(plat *platform.Platform) []proxySessionCandidate {
	if plat == nil {
		return nil
	}
	view := plat.View()
	candidates := make([]proxySessionCandidate, 0, view.Size())
	view.Range(func(h node.Hash) bool {
		entry, ok := m.cfg.Pool.GetEntry(h)
		if !ok || entry == nil || !entry.IsHealthy() {
			return true
		}
		ip := entry.GetEgressIP()
		if !ip.IsValid() {
			return true
		}
		candidates = append(candidates, proxySessionCandidate{hash: h, egressIP: ip})
		return true
	})
	sort.Slice(candidates, func(i, j int) bool {
		return candidates[i].hash.Hex() < candidates[j].hash.Hex()
	})
	return candidates
}

func (m *ProxySessionManager) listenRandomPort() (net.Listener, int, error) {
	if m.cfg.MinPort <= 0 || m.cfg.MaxPort < m.cfg.MinPort {
		return nil, 0, ErrProxySessionPortUnavailable
	}
	span := m.cfg.MaxPort - m.cfg.MinPort + 1
	for i := 0; i < m.cfg.PortRetries; i++ {
		offset, err := randomInt(span)
		if err != nil {
			return nil, 0, err
		}
		port := m.cfg.MinPort + offset
		ln, err := net.Listen("tcp", net.JoinHostPort(m.cfg.BindHost, strconv.Itoa(port)))
		if err == nil {
			return ln, port, nil
		}
	}
	return nil, 0, ErrProxySessionPortUnavailable
}

func randomInt(max int) (int, error) {
	if max <= 0 {
		return 0, ErrProxySessionPortUnavailable
	}
	n, err := rand.Int(rand.Reader, big.NewInt(int64(max)))
	if err != nil {
		return 0, fmt.Errorf("random port: %w", err)
	}
	return int(n.Int64()), nil
}

// Shutdown releases all sessions. It matches http.Server-style shutdown hooks.
func (m *ProxySessionManager) Shutdown(_ context.Context) error {
	m.Close()
	return nil
}

package routing

import (
	"crypto/sha256"
	"encoding/hex"
	"log"
	"net/netip"
	"sync"
	"time"

	"github.com/Resinat/Resin/internal/netutil"
)

const (
	targetEgressFailureWindow = 30 * time.Second
	targetEgressOpenDuration  = 2 * time.Minute
	targetEgressEntryTTL      = 10 * time.Minute
	targetEgressMaxEntries    = 16_384
)

type targetEgressCircuitKey struct {
	platformID   string
	egressIP     netip.Addr
	targetDomain string
}

type targetEgressCircuitState struct {
	failureCount int
	windowStart  time.Time
	openUntil    time.Time
	lastAccess   time.Time
}

type failedLeaseEgress struct {
	egressIP  netip.Addr
	expiresAt time.Time
}

// TargetEgressStats is the current in-memory target-egress circuit snapshot.
type TargetEgressStats struct {
	Open              int
	FailoverAttempts  uint64
	FailoverSuccesses uint64
	FailoverFailures  uint64
}

type targetEgressCircuitTracker struct {
	mu                sync.Mutex
	now               func() time.Time
	maxEntries        int
	entries           map[targetEgressCircuitKey]targetEgressCircuitState
	failedLeases      map[string]failedLeaseEgress
	failoverAttempts  uint64
	failoverSuccesses uint64
	failoverFailures  uint64
}

// newTargetEgressCircuitTracker creates a bounded process-local circuit tracker.
func newTargetEgressCircuitTracker(now func() time.Time, maxEntries int) *targetEgressCircuitTracker {
	if now == nil {
		now = time.Now
	}
	if maxEntries <= 0 {
		maxEntries = targetEgressMaxEntries
	}
	return &targetEgressCircuitTracker{
		now:          now,
		maxEntries:   maxEntries,
		entries:      make(map[targetEgressCircuitKey]targetEgressCircuitState),
		failedLeases: make(map[string]failedLeaseEgress),
	}
}

// markFailedLeaseEgress remembers the failed sticky egress until the account is rerouted once.
func (t *targetEgressCircuitTracker) markFailedLeaseEgress(platformID, account string, egressIP netip.Addr) {
	if t == nil || platformID == "" || account == "" || !egressIP.IsValid() {
		return
	}
	t.mu.Lock()
	defer t.mu.Unlock()
	now := t.now()
	t.cleanupLocked(now)
	t.failedLeases[platformID+"\x00"+account] = failedLeaseEgress{
		egressIP:  egressIP,
		expiresAt: now.Add(targetEgressOpenDuration),
	}
	for len(t.failedLeases) > t.maxEntries {
		var oldestKey string
		var oldest time.Time
		for key, state := range t.failedLeases {
			if oldest.IsZero() || state.expiresAt.Before(oldest) {
				oldestKey = key
				oldest = state.expiresAt
			}
		}
		delete(t.failedLeases, oldestKey)
	}
}

// failedLeaseEgress returns the short-lived egress excluded for the account's next route.
func (t *targetEgressCircuitTracker) failedLeaseEgress(platformID, account string) (netip.Addr, bool) {
	if t == nil || platformID == "" || account == "" {
		return netip.Addr{}, false
	}
	key := platformID + "\x00" + account
	now := t.now()
	t.mu.Lock()
	defer t.mu.Unlock()
	state, ok := t.failedLeases[key]
	if !ok {
		return netip.Addr{}, false
	}
	if !now.Before(state.expiresAt) {
		delete(t.failedLeases, key)
		return netip.Addr{}, false
	}
	return state.egressIP, true
}

// clearFailedLeaseEgress clears the one-shot exclusion after a different egress is selected.
func (t *targetEgressCircuitTracker) clearFailedLeaseEgress(platformID, account string) {
	if t == nil || platformID == "" || account == "" {
		return
	}
	t.mu.Lock()
	delete(t.failedLeases, platformID+"\x00"+account)
	t.mu.Unlock()
}

// recordFailure records one target-specific transport failure and reports whether it opened the circuit.
func (t *targetEgressCircuitTracker) recordFailure(platformID string, egressIP netip.Addr, target, failureKind string) bool {
	if t == nil || platformID == "" || !egressIP.IsValid() {
		return false
	}
	now := t.now()
	key := targetEgressKey(platformID, egressIP, target)
	if key.targetDomain == "" {
		return false
	}

	t.mu.Lock()
	defer t.mu.Unlock()
	t.cleanupLocked(now)
	state := t.entries[key]
	state.lastAccess = now
	if state.windowStart.IsZero() || now.Sub(state.windowStart) > targetEgressFailureWindow {
		state.windowStart = now
		state.failureCount = 1
	} else {
		state.failureCount++
	}
	opened := false
	if state.failureCount >= 2 {
		state.openUntil = now.Add(targetEgressOpenDuration)
		state.failureCount = 0
		state.windowStart = time.Time{}
		opened = true
		log.Printf(
			"target_egress_circuit action=open platform_id=%s target_domain=%s egress_hash=%s seconds=%d failure_kind=%s",
			platformID,
			key.targetDomain,
			hashEgressIP(egressIP),
			int(targetEgressOpenDuration.Seconds()),
			failureKind,
		)
	}
	t.entries[key] = state
	t.enforceBoundLocked()
	return opened
}

// recordSuccess clears target-specific failures after a complete network transfer.
func (t *targetEgressCircuitTracker) recordSuccess(platformID string, egressIP netip.Addr, target string) {
	if t == nil || platformID == "" || !egressIP.IsValid() {
		return
	}
	key := targetEgressKey(platformID, egressIP, target)
	if key.targetDomain == "" {
		return
	}
	t.mu.Lock()
	_, existed := t.entries[key]
	delete(t.entries, key)
	t.mu.Unlock()
	if existed {
		log.Printf(
			"target_egress_circuit action=reset platform_id=%s target_domain=%s egress_hash=%s",
			platformID,
			key.targetDomain,
			hashEgressIP(egressIP),
		)
	}
}

// isOpen reports whether the egress is temporarily unavailable for the target domain.
func (t *targetEgressCircuitTracker) isOpen(platformID string, egressIP netip.Addr, target string) bool {
	if t == nil || platformID == "" || !egressIP.IsValid() {
		return false
	}
	key := targetEgressKey(platformID, egressIP, target)
	now := t.now()
	t.mu.Lock()
	defer t.mu.Unlock()
	state, ok := t.entries[key]
	if !ok {
		return false
	}
	state.lastAccess = now
	if !state.openUntil.IsZero() && !now.Before(state.openUntil) {
		state.openUntil = time.Time{}
		state.failureCount = 0
		state.windowStart = time.Time{}
		t.entries[key] = state
		log.Printf(
			"target_egress_circuit action=expire platform_id=%s target_domain=%s egress_hash=%s",
			platformID,
			key.targetDomain,
			hashEgressIP(egressIP),
		)
		return false
	}
	t.entries[key] = state
	return !state.openUntil.IsZero()
}

// hasOpen reports whether filtering is needed for a platform and target domain.
func (t *targetEgressCircuitTracker) hasOpen(platformID, target string) bool {
	if t == nil || platformID == "" {
		return false
	}
	domain := netutil.ExtractDomain(target)
	now := t.now()
	t.mu.Lock()
	defer t.mu.Unlock()
	t.cleanupLocked(now)
	for key, state := range t.entries {
		if key.platformID == platformID && key.targetDomain == domain &&
			!state.openUntil.IsZero() && now.Before(state.openUntil) {
			return true
		}
	}
	return false
}

// cleanup removes inactive circuit entries.
func (t *targetEgressCircuitTracker) cleanup() {
	if t == nil {
		return
	}
	t.mu.Lock()
	t.cleanupLocked(t.now())
	t.mu.Unlock()
}

// entryCount returns the number of retained circuit entries for tests and diagnostics.
func (t *targetEgressCircuitTracker) entryCount() int {
	if t == nil {
		return 0
	}
	t.mu.Lock()
	defer t.mu.Unlock()
	return len(t.entries)
}

// snapshot returns current open circuits and cumulative failover counters.
func (t *targetEgressCircuitTracker) snapshot() TargetEgressStats {
	if t == nil {
		return TargetEgressStats{}
	}
	now := t.now()
	t.mu.Lock()
	defer t.mu.Unlock()
	t.cleanupLocked(now)
	open := 0
	for _, state := range t.entries {
		if !state.openUntil.IsZero() && now.Before(state.openUntil) {
			open++
		}
	}
	return TargetEgressStats{
		Open:              open,
		FailoverAttempts:  t.failoverAttempts,
		FailoverSuccesses: t.failoverSuccesses,
		FailoverFailures:  t.failoverFailures,
	}
}

// recordFailover updates cumulative CONNECT pre-commit failover counters.
func (t *targetEgressCircuitTracker) recordFailover(success *bool) {
	if t == nil {
		return
	}
	t.mu.Lock()
	defer t.mu.Unlock()
	if success == nil {
		t.failoverAttempts++
	} else if *success {
		t.failoverSuccesses++
	} else {
		t.failoverFailures++
	}
}

func (t *targetEgressCircuitTracker) cleanupLocked(now time.Time) {
	for key, state := range t.entries {
		if now.Sub(state.lastAccess) > targetEgressEntryTTL {
			delete(t.entries, key)
		}
	}
	for key, state := range t.failedLeases {
		if !now.Before(state.expiresAt) {
			delete(t.failedLeases, key)
		}
	}
}

func (t *targetEgressCircuitTracker) enforceBoundLocked() {
	for len(t.entries) > t.maxEntries {
		var oldestKey targetEgressCircuitKey
		var oldest time.Time
		for key, state := range t.entries {
			if oldest.IsZero() || state.lastAccess.Before(oldest) {
				oldestKey = key
				oldest = state.lastAccess
			}
		}
		delete(t.entries, oldestKey)
	}
}

func targetEgressKey(platformID string, egressIP netip.Addr, target string) targetEgressCircuitKey {
	return targetEgressCircuitKey{
		platformID:   platformID,
		egressIP:     egressIP,
		targetDomain: netutil.ExtractDomain(target),
	}
}

func hashEgressIP(ip netip.Addr) string {
	sum := sha256.Sum256([]byte(ip.String()))
	return hex.EncodeToString(sum[:6])
}

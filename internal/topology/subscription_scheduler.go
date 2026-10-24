package topology

import (
	"context"
	"errors"
	"log"
	"sync"
	"time"

	"github.com/Resinat/Resin/internal/netutil"
	"github.com/Resinat/Resin/internal/node"
	"github.com/Resinat/Resin/internal/scanloop"
	"github.com/Resinat/Resin/internal/subscription"
)

// SubscriptionScheduler manages periodic subscription updates.
type SubscriptionScheduler struct {
	subManager             *SubscriptionManager
	pool                   *GlobalNodePool
	downloader             netutil.Downloader
	downloadCtx            context.Context
	cancelDownload         context.CancelFunc
	refreshes              refreshCoordinator
	maxRefreshes           int
	maxHostRefreshes       int
	subscriptionDownloader *netutil.SubscriptionDownloader

	// Fetcher fetches subscription data from a URL.
	// Defaults to downloader.Download; injectable for testing.
	Fetcher func(url string) ([]byte, error)

	// For persistence.
	onSubUpdated func(sub *subscription.Subscription)
	// onSubReenabledNode is called for each non-evicted node hash when a
	// subscription transitions from disabled to enabled.
	onSubReenabledNode func(hash node.Hash)

	stopCh chan struct{}
	wg     sync.WaitGroup
}

// SchedulerConfig configures the SubscriptionScheduler.
type SchedulerConfig struct {
	SubscriptionDownloader     *netutil.SubscriptionDownloader
	MaxConcurrentRefreshes     int
	MaxConcurrentHostRefreshes int
	SubManager                 *SubscriptionManager
	Pool                       *GlobalNodePool
	Downloader                 netutil.Downloader               // shared downloader
	Fetcher                    func(url string) ([]byte, error) // optional, defaults to Downloader.Download
	OnSubUpdated               func(sub *subscription.Subscription)
	// OnSubReenabledNode is fired after false->true enabled transition.
	OnSubReenabledNode func(hash node.Hash)
}

// NewSubscriptionScheduler creates a new scheduler.
func NewSubscriptionScheduler(cfg SchedulerConfig) *SubscriptionScheduler {
	downloadCtx, cancelDownload := context.WithCancel(context.Background())
	sched := &SubscriptionScheduler{
		subManager:             cfg.SubManager,
		pool:                   cfg.Pool,
		downloader:             cfg.Downloader,
		downloadCtx:            downloadCtx,
		cancelDownload:         cancelDownload,
		onSubUpdated:           cfg.OnSubUpdated,
		onSubReenabledNode:     cfg.OnSubReenabledNode,
		stopCh:                 make(chan struct{}),
		refreshes:              newRefreshCoordinator(),
		maxRefreshes:           cfg.MaxConcurrentRefreshes,
		maxHostRefreshes:       cfg.MaxConcurrentHostRefreshes,
		subscriptionDownloader: cfg.SubscriptionDownloader,
	}
	if sched.maxRefreshes <= 0 {
		sched.maxRefreshes = 4
	}
	if sched.maxHostRefreshes <= 0 {
		sched.maxHostRefreshes = 2
	}
	if cfg.Fetcher != nil {
		sched.Fetcher = cfg.Fetcher
	} else {
		sched.Fetcher = sched.fetchViaDownloader
	}
	return sched
}

// Start launches the background scheduler goroutine.
func (s *SubscriptionScheduler) Start() {
	s.refreshes.Lock()
	if s.refreshes.closed {
		s.refreshes.Unlock()
		return
	}
	s.wg.Add(1)
	s.refreshes.Unlock()
	go func() {
		defer s.wg.Done()
		scanloop.Run(s.stopCh, scanloop.DefaultMinInterval, scanloop.DefaultJitterRange, s.tick)
	}()
}

// Stop signals the scheduler to stop and waits for it to finish.
func (s *SubscriptionScheduler) Stop() {
	s.refreshes.Lock()
	if s.refreshes.closed {
		s.refreshes.Unlock()
		return
	}
	s.refreshes.closed = true
	close(s.stopCh)
	s.refreshes.Unlock()
	s.cancelDownload()
	s.wg.Wait()
}

// ForceRefreshAll unconditionally updates ALL enabled subscriptions, regardless
// of their next-check timestamps. Called once at startup to compensate for
// lost data from weak persistence (DESIGN.md step 8 batch 3).
// Updates run in parallel, and this method waits until all started updates exit.
func (s *SubscriptionScheduler) ForceRefreshAll() {
	select {
	case <-s.stopCh:
		return
	default:
	}

	subsToRefresh := make([]*subscription.Subscription, 0, s.subManager.Size())
	s.subManager.Range(func(id string, sub *subscription.Subscription) bool {
		select {
		case <-s.stopCh:
			return false
		default:
		}
		if sub.Enabled() {
			subsToRefresh = append(subsToRefresh, sub)
		}
		return true
	})
	s.runUpdatesWithWorkerLimit(subsToRefresh)
}

// ForceRefreshAllAsync triggers ForceRefreshAll in a background goroutine.
// The goroutine is tracked by scheduler waitgroup so Stop() waits for exit.
func (s *SubscriptionScheduler) ForceRefreshAllAsync() {
	s.refreshes.Lock()
	if s.refreshes.closed {
		s.refreshes.Unlock()
		return
	}
	s.wg.Add(1)
	s.refreshes.Unlock()
	go func() {
		defer s.wg.Done()
		s.ForceRefreshAll()
	}()
}

func (s *SubscriptionScheduler) tick() {
	select {
	case <-s.stopCh:
		return
	default:
	}

	now := time.Now().UnixNano()
	dueSubs := make([]*subscription.Subscription, 0, s.subManager.Size())
	s.subManager.Range(func(id string, sub *subscription.Subscription) bool {
		select {
		case <-s.stopCh:
			return false
		default:
		}
		if !sub.Enabled() {
			return true
		}
		// Retry-After and failure backoff are exact lower bounds.
		if sub.RefreshDue(time.Unix(0, now)) {
			dueSubs = append(dueSubs, sub)
		}
		return true
	})
	s.runRefreshesAsync(dueSubs, false)
}

func (s *SubscriptionScheduler) runUpdatesWithWorkerLimit(subs []*subscription.Subscription) {
	s.runRefreshes(subs, true)
}

func (s *SubscriptionScheduler) runRefreshes(subs []*subscription.Subscription, force bool) {
	var wg sync.WaitGroup
	for _, sub := range subs {
		wg.Add(1)
		go func(sub *subscription.Subscription) {
			defer wg.Done()
			_ = s.refresh(context.Background(), sub, force)
		}(sub)
	}
	wg.Wait()
}

func (s *SubscriptionScheduler) runRefreshesAsync(subs []*subscription.Subscription, force bool) {
	for _, sub := range subs {
		// Start only new flights. Joining an existing flight would create a
		// blocked waiter goroutine for every periodic scan of the same sub.
		_, _ = s.startRefresh(context.Background(), sub, force)
	}
}

// UpdateSubscription fetches and parses outside the lock, then diffs and
// applies the result under WithSubLock. This keeps the lock scope narrow
// (no I/O under lock) while still preventing concurrent diff/apply races.
func (s *SubscriptionScheduler) updateSubscription(ctx context.Context, sub *subscription.Subscription) error {
	var attempts []netutil.DownloadAttempt
	attemptStartedNs := time.Now().UnixNano()
	attemptSeq := sub.NextAttemptSeq()
	inputs := sub.FetchInputs()
	attemptURL := inputs.URL
	attemptSourceType := inputs.Source
	attemptContent := inputs.Content
	attemptRelayPlatformID := inputs.RelayPlatformID
	attemptConfigVersion := inputs.Version

	// 1. Fetch/read content (lock-free).
	var (
		body []byte
		err  error
	)
	if attemptSourceType == subscription.SourceTypeLocal {
		body = []byte(attemptContent)
	} else {
		if s.subscriptionDownloader != nil {
			body, attempts, err = s.subscriptionDownloader.DownloadSubscription(ctx, attemptURL, inputs.DownloadMode, inputs.DownloadPlatformID)
		} else {
			body, err = s.Fetcher(attemptURL)
		}
		if err != nil {
			s.handleUpdateFailure(sub, attemptStartedNs, attemptSeq, attemptConfigVersion, "fetch", err, attempts)
			return err
		}
	}

	// 2. Parse (lock-free).
	parsed, err := subscription.ParseGeneralSubscription(body)
	if err != nil {
		s.handleUpdateFailure(sub, attemptStartedNs, attemptSeq, attemptConfigVersion, "parse", err, attempts)
		return &netutil.SubscriptionFetchError{Kind: "parse", Message: "subscription content could not be parsed"}
	}
	if len(parsed) == 0 {
		s.handleUpdateFailure(sub, attemptStartedNs, attemptSeq, attemptConfigVersion, "parse", errors.New("subscription parsed zero supported nodes"), attempts)
		return &netutil.SubscriptionFetchError{Kind: "parse", Message: "subscription parsed zero supported nodes"}
	}

	// 3. Build refreshed managed nodes map (lock-free, pure computation).
	refreshedManagedNodes := subscription.NewManagedNodes()
	rawByHash := make(map[node.Hash][]byte)
	for _, p := range parsed {
		h := node.HashFromRawOptionsWithRelayPlatform(p.RawOptions, attemptRelayPlatformID)
		existing, _ := refreshedManagedNodes.LoadNode(h)
		existing.Tags = append(existing.Tags, p.Tag)
		refreshedManagedNodes.StoreNode(h, existing)
		if _, ok := rawByHash[h]; !ok {
			rawByHash[h] = p.RawOptions
		}
	}

	// 4. Diff, swap, add/remove — under lock.
	applied := false
	sub.WithOpLock(func() {
		// If refresh-input config changed while this attempt was in-flight, discard.
		if sub.ConfigVersion() != attemptConfigVersion || !sub.Enabled() || s.subManager.Lookup(sub.ID) != sub || ctx.Err() != nil {
			return
		}
		// Stale success guard: if a newer successful update has already landed,
		// discard this older attempt to avoid rolling state backward.
		if sub.LastAppliedSeq() > attemptSeq {
			return
		}

		old := sub.ManagedNodes()
		mergedManagedNodes := refreshedManagedNodes
		if sub.IncrementalAliveNodes() {
			mergedManagedNodes = subscription.NewManagedNodes()
			old.RangeNodes(func(h node.Hash, oldNode subscription.ManagedNode) bool {
				// A relay Platform change changes node identity and must replace the
				// previous scope even when incremental mode is enabled.
				entry, entryOK := s.pool.GetEntry(h)
				if !entryOK || node.HashFromRawOptionsWithRelayPlatform(entry.RawOptions, attemptRelayPlatformID) != h {
					return true
				}
				if oldNode.Evicted {
					mergedManagedNodes.StoreNode(h, oldNode)
					return true
				}
				if !shouldRemoveUnhealthyNodeForIncrementalMode(entry) {
					mergedManagedNodes.StoreNode(h, oldNode)
				}
				return true
			})
			refreshedManagedNodes.RangeNodes(func(h node.Hash, refreshedNode subscription.ManagedNode) bool {
				mergedManagedNodes.StoreNode(h, refreshedNode)
				return true
			})
		}

		// Keep hashes inherit historical eviction state so refresh will not
		// re-add previously evicted nodes back into pool.
		old.RangeNodes(func(h node.Hash, oldNode subscription.ManagedNode) bool {
			if !oldNode.Evicted {
				return true
			}
			nextNode, ok := mergedManagedNodes.LoadNode(h)
			if !ok {
				return true
			}
			nextNode.Evicted = true
			mergedManagedNodes.StoreNode(h, nextNode)
			return true
		})
		added, kept, removed := subscription.DiffHashes(old, mergedManagedNodes)

		sub.SwapManagedNodes(mergedManagedNodes)

		for _, h := range added {
			managed, ok := mergedManagedNodes.LoadNode(h)
			if ok && managed.Evicted {
				continue
			}
			raw := rawByHash[h]
			if len(raw) == 0 {
				if entry, ok := s.pool.GetEntry(h); ok {
					raw = entry.RawOptions
				}
			}
			if len(raw) == 0 {
				continue
			}
			s.pool.AddNodeFromSub(h, raw, sub.ID)
		}
		for _, h := range kept {
			managed, ok := mergedManagedNodes.LoadNode(h)
			if ok && managed.Evicted {
				continue
			}
			raw := rawByHash[h]
			if len(raw) == 0 {
				if entry, ok := s.pool.GetEntry(h); ok {
					raw = entry.RawOptions
				}
			}
			if len(raw) == 0 {
				continue
			}
			s.pool.AddNodeFromSub(h, raw, sub.ID)
		}
		for _, h := range removed {
			s.pool.RemoveNodeFromSub(h, sub.ID)
		}
		// 5. Update timestamps (inside lock, using current time).
		now := time.Now().UnixNano()
		sub.LastCheckedNs.Store(now)
		sub.LastUpdatedNs.Store(now)
		sub.MarkAppliedAttempt(attemptSeq)
		sub.RestoreRefreshState(subscription.RefreshState{LastCheckedNs: now, LastUpdatedNs: now, NextRefreshNs: now + sub.UpdateIntervalNs(), Attempts: attempts})
		applied = true
	})
	if !applied {
		log.Printf("[scheduler] stale success ignored for %s", sub.ID)
		return errRefreshSuperseded
	}

	if s.onSubUpdated != nil {
		s.onSubUpdated(sub)
	}
	return nil
}

// handleUpdateFailure applies a fetch/parse failure to subscription state.
// It ignores stale failures from an outdated attempt (config-version guard +
// LastUpdatedNs stale-success guard).

func shouldRemoveUnhealthyNodeForIncrementalMode(entry *node.NodeEntry) bool {
	if entry == nil {
		return false
	}
	return entry.IsCircuitOpen() || (!entry.HasOutbound() && entry.GetLastError() != "")
}

func (s *SubscriptionScheduler) handleUpdateFailure(
	sub *subscription.Subscription,
	attemptStartedNs int64,
	attemptSeq int64,
	attemptConfigVersion int64,
	stage string,
	err error,
	attempts []netutil.DownloadAttempt,
) {
	if errors.Is(err, context.Canceled) || s.downloadCtx.Err() != nil {
		return
	}
	if stage == "parse" {
		err = &netutil.SubscriptionFetchError{Kind: "parse", Message: "subscription content has no valid supported nodes"}
		if s.subscriptionDownloader != nil {
			s.subscriptionDownloader.Invalidate()
		}
	}
	safeMessage := netutil.RedactErrorMessage(err.Error())
	applied := false
	sub.WithOpLock(func() {
		// If refresh-input config changed while this attempt was in-flight, discard.
		if sub.ConfigVersion() != attemptConfigVersion || !sub.Enabled() || s.subManager.Lookup(sub.ID) != sub {
			return
		}
		if sub.LastAppliedSeq() > attemptSeq {
			return
		}
		s.recordRefreshFailure(sub, err, attempts)
		applied = true
	})
	if !applied {
		log.Printf("[scheduler] stale %s failure ignored for %s: %s", stage, sub.ID, safeMessage)
		return
	}

	log.Printf("[scheduler] %s %s failed: %s", stage, sub.ID, safeMessage)
	if s.onSubUpdated != nil {
		s.onSubUpdated(sub)
	}
}

// SetSubscriptionEnabled updates the enabled flag and rebuilds all platform
// routable views. Disabling a subscription makes its nodes invisible to
// platform tag-regex matching; enabling makes them visible again.
func (s *SubscriptionScheduler) SetSubscriptionEnabled(sub *subscription.Subscription, enabled bool) {
	wasEnabled := false
	var candidateHashes []node.Hash
	wasDisabled := make(map[node.Hash]struct{})
	sub.WithOpLock(func() {
		wasEnabled = sub.Enabled()

		if !wasEnabled && enabled {
			sub.ManagedNodes().RangeNodes(func(h node.Hash, managed subscription.ManagedNode) bool {
				if managed.Evicted {
					return true
				}
				candidateHashes = append(candidateHashes, h)
				if s.pool != nil && s.pool.IsNodeDisabled(h) {
					wasDisabled[h] = struct{}{}
				}
				return true
			})
		}

		sub.SetEnabled(enabled)
	})
	s.CancelRefresh(sub)
	// Rebuild all platform views so they pick up the visibility change.
	s.pool.RebuildAllPlatforms()

	// On re-enable, immediately re-check node outbound/probe state for nodes
	// that actually transitioned from disabled -> enabled.
	if !wasEnabled && enabled && s.onSubReenabledNode != nil {
		for _, h := range candidateHashes {
			if _, ok := wasDisabled[h]; !ok {
				continue
			}
			if s.pool.IsNodeDisabled(h) {
				continue
			}
			s.onSubReenabledNode(h)
		}
	}

	if s.onSubUpdated != nil {
		s.onSubUpdated(sub)
	}
}

// SetSubscriptionRelayPlatformID applies a relay scope change and immediately
// removes the subscription's old node identities. This keeps the new setting
// fail-closed while the replacement refresh is running or if that refresh fails.
func (s *SubscriptionScheduler) SetSubscriptionRelayPlatformID(
	sub *subscription.Subscription,
	relayPlatformID string,
) {
	if sub == nil || sub.RelayPlatformID() == relayPlatformID {
		return
	}

	sub.WithOpLock(func() {
		if sub.RelayPlatformID() == relayPlatformID {
			return
		}
		oldManagedNodes := sub.ManagedNodes()
		hashes := make([]node.Hash, 0, oldManagedNodes.Size())
		oldManagedNodes.RangeNodes(func(hash node.Hash, _ subscription.ManagedNode) bool {
			hashes = append(hashes, hash)
			return true
		})

		// Publish the empty view before removing pool references so tag and
		// enabled-state lookups observe one consistent fail-closed state.
		sub.SetRelayPlatformID(relayPlatformID)
		sub.SwapManagedNodes(subscription.NewManagedNodes())
		for _, hash := range hashes {
			s.pool.RemoveNodeFromSub(hash, sub.ID)
		}
	})

	if s.onSubUpdated != nil {
		s.onSubUpdated(sub)
	}
}

// RenameSubscription updates the subscription name and re-triggers platform
// re-evaluation for all managed nodes (since tags include the sub name).
func (s *SubscriptionScheduler) RenameSubscription(sub *subscription.Subscription, newName string) {
	sub.WithOpLock(func() {
		sub.SetName(newName)
		// Re-add all managed hashes to trigger platform re-filter.
		sub.ManagedNodes().RangeNodes(func(h node.Hash, managed subscription.ManagedNode) bool {
			if managed.Evicted {
				return true
			}
			entry, ok := s.pool.GetEntry(h)
			if ok {
				s.pool.AddNodeFromSub(h, entry.RawOptions, sub.ID)
			}
			return true
		})
	})
}

func (s *SubscriptionScheduler) fetchViaDownloader(url string) ([]byte, error) {
	return s.downloader.Download(s.downloadCtx, url)
}

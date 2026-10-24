package topology

import (
	"context"
	"errors"
	"math/rand/v2"
	"net/url"
	"strings"
	"sync"
	"time"

	"github.com/Resinat/Resin/internal/netutil"
	"github.com/Resinat/Resin/internal/subscription"
)

var errRefreshSuperseded = errors.New("subscription refresh superseded")

type refreshFlight struct {
	done     chan struct{}
	err      error
	cancel   context.CancelFunc
	baseCtx  context.Context
	stopBase func()
	version  int64
}

type refreshCoordinator struct {
	sync.Mutex
	flights map[string]*refreshFlight
	active  int
	hosts   map[string]int
	changed chan struct{}
	closed  bool
}

func newRefreshCoordinator() refreshCoordinator {
	return refreshCoordinator{flights: make(map[string]*refreshFlight), hosts: make(map[string]int), changed: make(chan struct{})}
}

// UpdateSubscription joins any in-flight refresh for this subscription.
func (s *SubscriptionScheduler) UpdateSubscription(sub *subscription.Subscription) error {
	return s.UpdateSubscriptionContext(context.Background(), sub)
}

// UpdateSubscriptionContext is the cancellation-aware manual refresh entrypoint.
func (s *SubscriptionScheduler) UpdateSubscriptionContext(ctx context.Context, sub *subscription.Subscription) error {
	return s.refresh(ctx, sub, true)
}

// startRefresh creates at most one flight for a subscription. Callers that do
// not need the result can use the returned flight without creating a waiter
// goroutine for an already-running refresh.
func (s *SubscriptionScheduler) startRefresh(ctx context.Context, sub *subscription.Subscription, force bool) (*refreshFlight, bool) {
	c := &s.refreshes
	c.Lock()
	if c.closed {
		c.Unlock()
		return nil, false
	}
	if current := c.flights[sub.ID]; current != nil {
		c.Unlock()
		return current, false
	}
	if ctx == nil {
		ctx = context.Background()
	}
	baseCtx, baseCancel := context.WithCancel(s.downloadCtx)
	f := &refreshFlight{
		done:     make(chan struct{}),
		baseCtx:  baseCtx,
		stopBase: baseCancel,
	}
	c.flights[sub.ID] = f
	sub.SetRefreshActivity("queued")
	s.wg.Add(1)
	c.Unlock()
	go func() {
		defer s.wg.Done()
		for {
			f.err = s.executeRefresh(sub, force, f)
			c.Lock()
			if !c.closed && sub.Enabled() && s.subManager.Lookup(sub.ID) == sub && sub.ConfigVersion() != f.version {
				c.Unlock()
				force = true
				continue
			}
			break
		}
		sub.SetRefreshActivity("")
		delete(c.flights, sub.ID)
		close(f.done)
		c.Unlock()
		if f.stopBase != nil {
			f.stopBase()
		}
	}()
	return f, true
}

func (s *SubscriptionScheduler) refresh(ctx context.Context, sub *subscription.Subscription, force bool) error {
	if ctx == nil {
		ctx = context.Background()
	}
	f, started := s.startRefresh(ctx, sub, force)
	if f == nil {
		return context.Canceled
	}
	if started {
		select {
		case <-f.done:
			return f.err
		case <-ctx.Done():
			return ctx.Err()
		}
	}
	select {
	case <-f.done:
		return f.err
	case <-ctx.Done():
		return ctx.Err()
	}
}

func (s *SubscriptionScheduler) acquireRefresh(ctx context.Context, host string) error {
	c := &s.refreshes
	for {
		c.Lock()
		if c.closed || ctx.Err() != nil {
			c.Unlock()
			return context.Canceled
		}
		if c.active < s.maxRefreshes && c.hosts[host] < s.maxHostRefreshes {
			c.active++
			c.hosts[host]++
			c.Unlock()
			return nil
		}
		changed := c.changed
		c.Unlock()
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-changed:
		}
	}
}

func (s *SubscriptionScheduler) releaseRefresh(host string) {
	c := &s.refreshes
	c.Lock()
	defer c.Unlock()
	c.active--
	c.hosts[host]--
	if c.hosts[host] == 0 {
		delete(c.hosts, host)
	}
	close(c.changed)
	c.changed = make(chan struct{})
}

func (s *SubscriptionScheduler) executeRefresh(sub *subscription.Subscription, force bool, f *refreshFlight) error {
	for {
		if s.downloadCtx.Err() != nil || !sub.Enabled() || s.subManager.Lookup(sub.ID) != sub {
			return context.Canceled
		}
		version := sub.ConfigVersion()
		f.version = version
		u, _ := url.Parse(sub.URL())
		host := "local"
		if u != nil && u.Hostname() != "" {
			host = strings.ToLower(u.Hostname())
		}
		ctx, cancel := context.WithCancel(f.baseCtx)
		s.refreshes.Lock()
		f.cancel = cancel
		s.refreshes.Unlock()
		if err := s.acquireRefresh(ctx, host); err != nil {
			cancel()
			if version != sub.ConfigVersion() && sub.Enabled() && s.subManager.Lookup(sub.ID) == sub && s.downloadCtx.Err() == nil {
				force = true
				continue
			}
			return err
		}
		if version != sub.ConfigVersion() {
			s.releaseRefresh(host)
			cancel()
			continue
		}
		if !sub.Enabled() || s.subManager.Lookup(sub.ID) != sub {
			s.releaseRefresh(host)
			cancel()
			return context.Canceled
		}
		if !force && !sub.RefreshDue(time.Now()) {
			s.releaseRefresh(host)
			cancel()
			return nil
		}
		sub.SetRefreshActivity("updating")
		err := s.updateSubscription(ctx, sub)
		s.releaseRefresh(host)
		cancel()
		if sub.ConfigVersion() != version && sub.Enabled() && s.subManager.Lookup(sub.ID) == sub && s.downloadCtx.Err() == nil {
			force = true
			sub.SetRefreshActivity("queued")
			continue
		}
		return err
	}
}

// CancelRefresh invalidates active transport I/O after a configuration/lifecycle change.
func (s *SubscriptionScheduler) CancelRefresh(sub *subscription.Subscription) {
	s.refreshes.Lock()
	if f := s.refreshes.flights[sub.ID]; f != nil && f.cancel != nil {
		f.cancel()
	}
	s.refreshes.Unlock()
	if s.subscriptionDownloader != nil {
		s.subscriptionDownloader.Invalidate()
	}
}

func (s *SubscriptionScheduler) SetSubscriptionDownloader(d *netutil.SubscriptionDownloader) {
	s.subscriptionDownloader = d
}

func subscriptionRetryDelay(failures int, interval time.Duration, retryAfter time.Duration) time.Duration {
	delays := []time.Duration{time.Minute, 2 * time.Minute, 5 * time.Minute, 15 * time.Minute}
	i := failures - 1
	if i < 0 {
		i = 0
	}
	if i >= len(delays) {
		i = len(delays) - 1
	}
	delay := time.Duration(float64(delays[i]) * (0.8 + rand.Float64()*0.4))
	if delay > 15*time.Minute {
		delay = 15 * time.Minute
	}
	if interval > 0 && delay > interval {
		delay = interval
	}
	if retryAfter > delay {
		delay = retryAfter
	}
	return delay
}

func (s *SubscriptionScheduler) recordRefreshFailure(sub *subscription.Subscription, err error, attempts []netutil.DownloadAttempt) {
	failure := netutil.ClassifySubscriptionError(err)
	state := sub.RefreshSnapshot()
	state.LastCheckedNs = time.Now().UnixNano()
	state.LastError = failure.Message
	state.ErrorKind = failure.Kind
	state.ConsecutiveFailures++
	state.Attempts = attempts
	state.Retrying = failure.Retryable
	delay := time.Duration(sub.UpdateIntervalNs())
	if failure.Retryable {
		delay = subscriptionRetryDelay(state.ConsecutiveFailures, delay, failure.RetryAfter)
	}
	state.NextRefreshNs = time.Now().Add(delay).UnixNano()
	sub.RestoreRefreshState(state)
}

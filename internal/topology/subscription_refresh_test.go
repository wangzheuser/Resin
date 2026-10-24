package topology

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/Resinat/Resin/internal/netutil"
	"github.com/Resinat/Resin/internal/subscription"
)

func TestRefreshReliability_Deduplicates(t *testing.T) {
	mgr := NewSubscriptionManager()
	sub := subscription.NewSubscription("dedup", "dedup", "https://example.com/sub", true, false)
	sub.SetFetchConfig(sub.URL(), int64(12*time.Hour))
	mgr.Register(sub)
	started := make(chan struct{})
	release := make(chan struct{})
	var calls atomic.Int32
	sched := newTestScheduler(mgr, newTestPool(mgr), func(string) ([]byte, error) {
		if calls.Add(1) == 1 {
			close(started)
		}
		<-release
		return nil, errors.New("controlled failure")
	})
	defer sched.Stop()
	var wg sync.WaitGroup
	wg.Add(1)
	go func() { defer wg.Done(); sched.ForceRefreshAll() }()
	<-started
	wg.Add(2)
	go func() { defer wg.Done(); sched.tick() }()
	go func() { defer wg.Done(); sched.UpdateSubscription(sub) }()
	time.Sleep(50 * time.Millisecond)
	count := calls.Load()
	close(release)
	wg.Wait()
	if count != 1 {
		t.Fatalf("duplicate downloads: got %d, want 1", count)
	}
}

func TestRefreshReliability_CallerCancellationDoesNotCancelSharedFlight(t *testing.T) {
	mgr := NewSubscriptionManager()
	sub := subscription.NewSubscription("cancel", "cancel", "https://example.com/sub", true, false)
	sub.SetFetchConfig(sub.URL(), int64(time.Hour))
	mgr.Register(sub)
	started := make(chan struct{})
	release := make(chan struct{})
	var calls atomic.Int32
	sched := newTestScheduler(mgr, newTestPool(mgr), func(string) ([]byte, error) {
		if calls.Add(1) == 1 {
			close(started)
		}
		<-release
		return makeSubscriptionJSON(`{"type":"shadowsocks","server":"example.com","server_port":443}`), nil
	})
	defer sched.Stop()

	flight, flightStarted := sched.startRefresh(context.Background(), sub, true)
	if !flightStarted || flight == nil {
		t.Fatal("expected shared refresh flight to start")
	}
	ctx, cancel := context.WithCancel(context.Background())
	first := make(chan error, 1)
	go func() { first <- sched.UpdateSubscriptionContext(ctx, sub) }()
	<-started
	cancel()
	if err := <-first; !errors.Is(err, context.Canceled) {
		t.Fatalf("first caller error: got %v, want context.Canceled", err)
	}
	close(release)
	select {
	case <-flight.done:
	case <-time.After(time.Second):
		t.Fatal("shared refresh flight did not finish")
	}
	if flight.err != nil || calls.Load() != 1 || sub.LastUpdatedNs.Load() == 0 {
		t.Fatalf("shared refresh did not complete: err=%v calls=%d updated=%d", flight.err, calls.Load(), sub.LastUpdatedNs.Load())
	}
}

func TestRefreshReliability_BackoffAndRecovery(t *testing.T) {
	mgr := NewSubscriptionManager()
	sub := subscription.NewSubscription("retry", "retry", "https://example.com/sub", true, false)
	sub.SetFetchConfig(sub.URL(), int64(12*time.Hour))
	mgr.Register(sub)
	var calls atomic.Int32
	sched := newTestScheduler(mgr, newTestPool(mgr), func(string) ([]byte, error) {
		call := calls.Add(1)
		if call == 1 {
			return nil, context.DeadlineExceeded
		}
		return makeSubscriptionJSON(`{"type":"shadowsocks","server":"example.com","server_port":443}`), nil
	})
	defer sched.Stop()
	_ = sched.UpdateSubscription(sub)
	r := sub.RefreshSnapshot()
	delay := time.Duration(r.NextRefreshNs - r.LastCheckedNs)
	if r.ConsecutiveFailures != 1 || delay < 48*time.Second || delay > 73*time.Second || sub.RefreshStatus() != "retry_wait" {
		t.Fatalf("invalid retry state: %+v", r)
	}
	sched.tick()
	if calls.Load() != 1 {
		t.Fatal("retried before due")
	}
	r.NextRefreshNs = time.Now().Add(-time.Second).UnixNano()
	sub.RestoreRefreshState(r)
	sched.tick()
	deadline := time.Now().Add(time.Second)
	for calls.Load() < 2 && time.Now().Before(deadline) {
		time.Sleep(5 * time.Millisecond)
	}
	r = sub.RefreshSnapshot()
	if calls.Load() != 2 || r.ConsecutiveFailures != 0 || r.LastError != "" || sub.RefreshStatus() != "ok" {
		t.Fatalf("did not recover: calls=%d state=%+v", calls.Load(), r)
	}
	if r.NextRefreshNs-r.LastCheckedNs != int64(12*time.Hour) {
		t.Fatal("normal interval not restored")
	}
	if got := subscriptionRetryDelay(10, 30*time.Second, 2*time.Minute); got != 2*time.Minute {
		t.Fatal("Retry-After not respected")
	}
}

func TestRefreshReliability_GlobalAndHostLimits(t *testing.T) {
	mgr := NewSubscriptionManager()
	pool := newTestPool(mgr)
	for i := 0; i < 12; i++ {
		sub := subscription.NewSubscription(fmt.Sprint(i), "limit", fmt.Sprintf("https://host%d.test/sub", i%3), true, false)
		mgr.Register(sub)
	}
	var mu sync.Mutex
	active, maxGlobal := 0, 0
	hosts := map[string]int{}
	maxHost := 0
	sched := newTestScheduler(mgr, pool, func(target string) ([]byte, error) {
		mu.Lock()
		active++
		hosts[target]++
		if active > maxGlobal {
			maxGlobal = active
		}
		if hosts[target] > maxHost {
			maxHost = hosts[target]
		}
		mu.Unlock()
		time.Sleep(20 * time.Millisecond)
		mu.Lock()
		active--
		hosts[target]--
		mu.Unlock()
		return nil, errors.New("controlled")
	})
	defer sched.Stop()
	sched.ForceRefreshAll()
	if maxGlobal > 4 || maxGlobal < 2 || maxHost > 2 {
		t.Fatalf("global=%d host=%d", maxGlobal, maxHost)
	}
}

func TestRefreshReliability_ConfigChangeAndLifecycle(t *testing.T) {
	for _, action := range []string{"url", "disable", "delete"} {
		t.Run(action, func(t *testing.T) {
			mgr := NewSubscriptionManager()
			sub := subscription.NewSubscription("change", "change", "https://old.test/sub", true, false)
			mgr.Register(sub)
			started := make(chan struct{})
			release := make(chan struct{})
			var calls atomic.Int32
			sched := newTestScheduler(mgr, newTestPool(mgr), func(target string) ([]byte, error) {
				if calls.Add(1) == 1 {
					close(started)
					<-release
				}
				return makeSubscriptionJSON(fmt.Sprintf(`{"type":"shadowsocks","server":%q,"server_port":443}`, target)), nil
			})
			defer sched.Stop()
			done := make(chan struct{})
			go func() { defer close(done); _ = sched.UpdateSubscription(sub) }()
			<-started
			switch action {
			case "url":
				sub.SetFetchConfig("https://new.test/sub", int64(time.Hour))
			case "disable":
				sub.SetEnabled(false)
			case "delete":
				mgr.Unregister(sub.ID)
			}
			sched.CancelRefresh(sub)
			close(release)
			<-done
			if action == "url" {
				if calls.Load() != 2 || sub.LastUpdatedNs.Load() == 0 {
					t.Fatal("latest configuration not refreshed")
				}
			} else if sub.LastUpdatedNs.Load() != 0 || sub.RefreshSnapshot().ConsecutiveFailures != 0 {
				t.Fatal("cancelled update changed state")
			}
		})
	}
}

func TestRefreshReliability_PermanentFailureRetainsNodes(t *testing.T) {
	mgr := NewSubscriptionManager()
	sub := subscription.NewSubscription("retain", "retain", "https://example.com/sub", true, false)
	sub.SetFetchConfig(sub.URL(), int64(12*time.Hour))
	mgr.Register(sub)
	calls := 0
	pool := newTestPool(mgr)
	sched := newTestScheduler(mgr, pool, func(string) ([]byte, error) {
		calls++
		if calls == 1 {
			return makeSubscriptionJSON(`{"type":"shadowsocks","server":"example.com","server_port":443}`), nil
		}
		return nil, &netutil.HTTPStatusError{StatusCode: 400, URL: "https://example.com/private?token=secret"}
	})
	defer sched.Stop()
	_ = sched.UpdateSubscription(sub)
	_ = sched.UpdateSubscription(sub)
	r := sub.RefreshSnapshot()
	if pool.Size() != 1 || r.LastUpdatedNs == 0 || r.Retrying || r.NextRefreshNs-r.LastCheckedNs < int64(12*time.Hour) {
		t.Fatalf("invalid retained state: %+v", r)
	}
}

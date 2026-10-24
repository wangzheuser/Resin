package subscription

import (
	"time"

	"github.com/Resinat/Resin/internal/netutil"
)

// RefreshState is a copyable snapshot of the latest completed refresh.
// Active queue state is deliberately not restored after process restart.
type RefreshState struct {
	LastCheckedNs       int64                     `json:"last_checked_ns"`
	LastUpdatedNs       int64                     `json:"last_updated_ns"`
	LastError           string                    `json:"last_error"`
	ErrorKind           string                    `json:"error_kind"`
	ConsecutiveFailures int                       `json:"consecutive_failures"`
	NextRefreshNs       int64                     `json:"next_refresh_ns"`
	Retrying            bool                      `json:"retrying"`
	Attempts            []netutil.DownloadAttempt `json:"attempts"`
}

type FetchInputs struct {
	URL, Source, Content, RelayPlatformID, DownloadMode, DownloadPlatformID string
	Version                                                                 int64
}

func (s *Subscription) FetchInputs() FetchInputs {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return FetchInputs{URL: s.url, Source: normalizeSourceType(s.sourceType), Content: s.content, RelayPlatformID: s.relayPlatformID, DownloadMode: s.downloadMode, DownloadPlatformID: s.downloadPlatformID, Version: s.configVersion.Load()}
}

func (s *Subscription) DownloadConfig() (string, string) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	mode := s.downloadMode
	if mode == "" {
		mode = "auto"
	}
	return mode, s.downloadPlatformID
}

func (s *Subscription) SetDownloadConfig(mode, platformID string) {
	if mode == "" {
		mode = "auto"
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if mode != s.downloadMode || platformID != s.downloadPlatformID {
		s.downloadMode, s.downloadPlatformID = mode, platformID
		s.configVersion.Add(1)
	}
}

func (s *Subscription) RefreshSnapshot() RefreshState {
	s.refreshMu.RLock()
	defer s.refreshMu.RUnlock()
	r := s.refreshState
	r.Attempts = append([]netutil.DownloadAttempt(nil), r.Attempts...)
	r.LastCheckedNs = s.LastCheckedNs.Load()
	r.LastUpdatedNs = s.LastUpdatedNs.Load()
	r.LastError = s.GetLastError()
	return r
}

func (s *Subscription) RestoreRefreshState(r RefreshState) {
	s.refreshMu.Lock()
	defer s.refreshMu.Unlock()
	r.Attempts = append([]netutil.DownloadAttempt(nil), r.Attempts...)
	s.refreshState = r
	s.LastCheckedNs.Store(r.LastCheckedNs)
	s.LastUpdatedNs.Store(r.LastUpdatedNs)
	s.SetLastError(r.LastError)
}

func (s *Subscription) SetRefreshActivity(activity string) {
	s.refreshMu.Lock()
	s.refreshActivity = activity
	s.refreshMu.Unlock()
}

func (s *Subscription) RefreshStatus() string {
	if !s.Enabled() {
		return "disabled"
	}
	s.refreshMu.RLock()
	activity := s.refreshActivity
	retry := s.refreshState.Retrying
	s.refreshMu.RUnlock()
	if activity != "" {
		return activity
	}
	if s.GetLastError() != "" {
		if retry {
			return "retry_wait"
		}
		return "error"
	}
	if s.LastUpdatedNs.Load() > 0 {
		return "ok"
	}
	return "never"
}

func (s *Subscription) RefreshDue(now time.Time) bool {
	r := s.RefreshSnapshot()
	if r.NextRefreshNs > 0 {
		return now.UnixNano() >= r.NextRefreshNs
	}
	return r.LastCheckedNs+s.UpdateIntervalNs() <= now.UnixNano()
}

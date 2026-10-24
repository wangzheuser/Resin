package service

import (
	"context"
	"errors"
	"sort"
	"time"

	"github.com/Resinat/Resin/internal/node"
	"github.com/Resinat/Resin/internal/subscription"
	"github.com/google/uuid"
)

// SubscriptionProbeStatus is the latest in-memory job; it expires on restart.
type SubscriptionProbeStatus struct {
	ID               string `json:"id"`
	SubscriptionID   string `json:"subscription_id"`
	SubscriptionName string `json:"subscription_name"`
	Status           string `json:"status"`
	Total            int    `json:"total"`
	Completed        int    `json:"completed"`
	Succeeded        int    `json:"succeeded"`
	Failed           int    `json:"failed"`
	Skipped          int    `json:"skipped"`
	StartedAt        string `json:"started_at"`
	FinishedAt       string `json:"finished_at,omitempty"`
}

type subscriptionProbeJob struct {
	SubscriptionProbeStatus
	cancel context.CancelFunc
}

func (j *subscriptionProbeJob) active() bool {
	return j.Status == "running" || j.Status == "cancelling"
}

func (s *ControlPlaneService) SubscriptionProbeStatus() *SubscriptionProbeStatus {
	s.subscriptionProbeMu.Lock()
	defer s.subscriptionProbeMu.Unlock()
	if s.subscriptionProbe == nil {
		return nil
	}
	status := s.subscriptionProbe.SubscriptionProbeStatus
	return &status
}

func (s *ControlPlaneService) StartSubscriptionProbe(id string) (*SubscriptionProbeStatus, error) {
	sub := s.SubMgr.Lookup(id)
	if sub == nil {
		return nil, notFound("subscription not found")
	}
	if !sub.Enabled() {
		return nil, invalidArg("subscription is disabled")
	}
	if s.ProbeMgr == nil || s.Pool == nil {
		return nil, internal("probe manager not configured", nil)
	}
	s.subscriptionProbeMu.Lock()
	defer s.subscriptionProbeMu.Unlock()
	if current := s.subscriptionProbe; current != nil && current.active() {
		if current.SubscriptionID != id {
			return nil, conflict("another subscription probe is running")
		}
		status := current.SubscriptionProbeStatus
		return &status, nil
	}
	var hashes []node.Hash
	sub.ManagedNodes().RangeNodes(func(h node.Hash, managed subscription.ManagedNode) bool {
		if !managed.Evicted {
			hashes = append(hashes, h)
		}
		return true
	})
	sort.Slice(hashes, func(i, j int) bool { return hashes[i].Hex() < hashes[j].Hex() })
	ctx, cancel := context.WithCancel(context.Background())
	job := &subscriptionProbeJob{SubscriptionProbeStatus: SubscriptionProbeStatus{
		ID: uuid.NewString(), SubscriptionID: id, SubscriptionName: sub.Name(), Status: "running",
		Total: len(hashes), StartedAt: time.Now().UTC().Format(time.RFC3339Nano),
	}, cancel: cancel}
	s.subscriptionProbe = job
	status := job.SubscriptionProbeStatus
	go s.runSubscriptionProbe(ctx, job, hashes)
	return &status, nil
}

func (s *ControlPlaneService) CancelSubscriptionProbe(id string) (*SubscriptionProbeStatus, error) {
	s.subscriptionProbeMu.Lock()
	defer s.subscriptionProbeMu.Unlock()
	job := s.subscriptionProbe
	if job == nil || job.ID != id {
		return nil, notFound("subscription probe job not found")
	}
	if job.active() {
		job.Status = "cancelling"
		job.cancel()
	}
	status := job.SubscriptionProbeStatus
	return &status, nil
}

func (s *ControlPlaneService) runSubscriptionProbe(ctx context.Context, job *subscriptionProbeJob, hashes []node.Hash) {
	defer func() {
		s.subscriptionProbeMu.Lock()
		defer s.subscriptionProbeMu.Unlock()
		job.Status = "completed"
		if ctx.Err() != nil {
			job.Status = "cancelled"
		}
		job.FinishedAt = time.Now().UTC().Format(time.RFC3339Nano)
		job.cancel()
	}()
	for _, hash := range hashes {
		if ctx.Err() != nil {
			return
		}
		sub := s.SubMgr.Lookup(job.SubscriptionID)
		if sub == nil || !sub.Enabled() {
			job.cancel()
			return
		}
		managed, member := sub.ManagedNodes().LoadNode(hash)
		entry, exists := s.Pool.GetEntry(hash)
		skipped := !member || managed.Evicted || !exists || !entry.HasOutbound()
		success := false
		if !skipped {
			result, err := s.ProbeMgr.ProbeEgressQueued(ctx, hash)
			if ctx.Err() != nil || errors.Is(err, context.Canceled) {
				job.cancel()
				return
			}
			success = err == nil && result != nil && result.EgressIP != ""
		}
		s.subscriptionProbeMu.Lock()
		job.Completed++
		switch {
		case skipped:
			job.Skipped++
		case success:
			job.Succeeded++
		default:
			job.Failed++
		}
		s.subscriptionProbeMu.Unlock()
	}
}

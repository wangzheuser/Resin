package service

import (
	"cmp"
	"slices"
	"strings"
	"time"

	"github.com/Resinat/Resin/internal/model"
	"github.com/Resinat/Resin/internal/node"
	"github.com/Resinat/Resin/internal/routing"
)

// ------------------------------------------------------------------
// Leases
// ------------------------------------------------------------------

// LeaseResponse is the API response for a lease.
type LeaseResponse struct {
	PlatformID   string `json:"platform_id"`
	Account      string `json:"account"`
	NodeHash     string `json:"node_hash"`
	NodeTag      string `json:"node_tag"`
	EgressIP     string `json:"egress_ip"`
	Expiry       string `json:"expiry"`
	LastAccessed string `json:"last_accessed"`
}

// LeaseListOptions contains filters, sorting, and pagination for a lease list query.
type LeaseListOptions struct {
	Account   string
	Fuzzy     bool
	SortBy    string
	SortOrder string
	Limit     int
	Offset    int
}

type leaseListCandidate struct {
	account string
	lease   routing.Lease
}

func leaseToResponse(lease model.Lease, nodeTag string) LeaseResponse {
	return LeaseResponse{
		PlatformID:   lease.PlatformID,
		Account:      lease.Account,
		NodeHash:     lease.NodeHash,
		NodeTag:      nodeTag,
		EgressIP:     lease.EgressIP,
		Expiry:       time.Unix(0, lease.ExpiryNs).UTC().Format(time.RFC3339Nano),
		LastAccessed: time.Unix(0, lease.LastAccessedNs).UTC().Format(time.RFC3339Nano),
	}
}

func (s *ControlPlaneService) resolveLeaseNodeTag(hash node.Hash) string {
	if s == nil || s.Pool == nil {
		return ""
	}
	return s.Pool.ResolveNodeDisplayTag(hash)
}

func (s *ControlPlaneService) resolveLeaseNodeTagFromHex(hashHex string) string {
	hash, err := node.ParseHex(hashHex)
	if err != nil {
		return ""
	}
	return s.resolveLeaseNodeTag(hash)
}

// ListLeasesPage returns one filtered and sorted page of leases for a platform.
func (s *ControlPlaneService) ListLeasesPage(platformID string, options LeaseListOptions) ([]LeaseResponse, int, error) {
	if _, ok := s.Pool.GetPlatform(platformID); !ok {
		return nil, 0, notFound("platform not found")
	}

	account := strings.TrimSpace(options.Account)
	if account != "" && !options.Fuzzy {
		lease := s.Router.ReadLease(model.LeaseKey{PlatformID: platformID, Account: account})
		if lease == nil {
			return []LeaseResponse{}, 0, nil
		}
		if options.Offset > 0 || options.Limit <= 0 {
			return []LeaseResponse{}, 1, nil
		}
		return []LeaseResponse{
			leaseToResponse(*lease, s.resolveLeaseNodeTagFromHex(lease.NodeHash)),
		}, 1, nil
	}

	accountLower := strings.ToLower(account)
	candidates := make([]leaseListCandidate, 0, s.Router.LeaseCount(platformID))
	s.Router.RangeLeases(platformID, func(account string, lease routing.Lease) bool {
		if accountLower != "" && !strings.Contains(strings.ToLower(account), accountLower) {
			return true
		}
		candidates = append(candidates, leaseListCandidate{account: account, lease: lease})
		return true
	})

	slices.SortFunc(candidates, func(a, b leaseListCandidate) int {
		order := compareLeaseListCandidate(options.SortBy, a, b)
		if order != 0 && options.SortOrder == "desc" {
			order = -order
		}
		if order != 0 {
			return order
		}
		return strings.Compare(a.account, b.account)
	})

	total := len(candidates)
	if options.Offset >= total || options.Limit <= 0 {
		return []LeaseResponse{}, total, nil
	}
	end := total
	if options.Limit < total-options.Offset {
		end = options.Offset + options.Limit
	}

	page := candidates[options.Offset:end]
	result := make([]LeaseResponse, 0, len(page))
	for _, candidate := range page {
		lease := candidate.lease
		result = append(result, leaseToResponse(model.Lease{
			PlatformID:     platformID,
			Account:        candidate.account,
			NodeHash:       lease.NodeHash.Hex(),
			EgressIP:       lease.EgressIP.String(),
			ExpiryNs:       lease.ExpiryNs,
			LastAccessedNs: lease.LastAccessedNs,
		}, s.resolveLeaseNodeTag(lease.NodeHash)))
	}
	return result, total, nil
}

func compareLeaseListCandidate(sortBy string, a, b leaseListCandidate) int {
	switch sortBy {
	case "expiry":
		return cmp.Compare(a.lease.ExpiryNs, b.lease.ExpiryNs)
	case "last_accessed":
		return cmp.Compare(a.lease.LastAccessedNs, b.lease.LastAccessedNs)
	default:
		return strings.Compare(a.account, b.account)
	}
}

// GetLease returns a single lease.
func (s *ControlPlaneService) GetLease(platformID, account string) (*LeaseResponse, error) {
	if _, ok := s.Pool.GetPlatform(platformID); !ok {
		return nil, notFound("platform not found")
	}
	ml := s.Router.ReadLease(model.LeaseKey{PlatformID: platformID, Account: account})
	if ml == nil {
		return nil, notFound("lease not found")
	}
	resp := leaseToResponse(*ml, s.resolveLeaseNodeTagFromHex(ml.NodeHash))
	return &resp, nil
}

// InheritLeaseByPlatformName copies a valid parent lease onto newAccount.
func (s *ControlPlaneService) InheritLeaseByPlatformName(platformName, parentAccount, newAccount string) error {
	platformName = strings.TrimSpace(platformName)
	if platformName == "" {
		return invalidArg("platform: must be non-empty")
	}
	parentAccount = strings.TrimSpace(parentAccount)
	if parentAccount == "" {
		return invalidArg("parent_account: must be non-empty")
	}
	newAccount = strings.TrimSpace(newAccount)
	if newAccount == "" {
		return invalidArg("new_account: must be non-empty")
	}
	if parentAccount == newAccount {
		return invalidArg("new_account: must differ from parent_account")
	}

	plat, ok := s.Pool.GetPlatformByName(platformName)
	if !ok || plat == nil {
		return notFound("platform not found")
	}

	parentLease := s.Router.ReadLease(model.LeaseKey{
		PlatformID: plat.ID,
		Account:    parentAccount,
	})
	nowNs := time.Now().UnixNano()
	if parentLease == nil || parentLease.ExpiryNs < nowNs {
		return notFound("parent lease not found")
	}

	next := *parentLease
	next.Account = newAccount
	if err := s.Router.UpsertLease(next); err != nil {
		return internal("inherit lease", err)
	}

	return nil
}

// DeleteLease removes a single lease.
func (s *ControlPlaneService) DeleteLease(platformID, account string) error {
	if _, ok := s.Pool.GetPlatform(platformID); !ok {
		return notFound("platform not found")
	}
	if !s.Router.DeleteLease(platformID, account) {
		return notFound("lease not found")
	}
	return nil
}

// DeleteAllLeases removes all leases for a platform.
func (s *ControlPlaneService) DeleteAllLeases(platformID string) error {
	if _, ok := s.Pool.GetPlatform(platformID); !ok {
		return notFound("platform not found")
	}
	s.Router.DeleteAllLeases(platformID)
	return nil
}

// IPLoadEntry is the API response for IP load stats.
type IPLoadEntry struct {
	EgressIP   string `json:"egress_ip"`
	LeaseCount int64  `json:"lease_count"`
}

// GetIPLoad returns IP load stats for a platform.
func (s *ControlPlaneService) GetIPLoad(platformID string) ([]IPLoadEntry, error) {
	if _, ok := s.Pool.GetPlatform(platformID); !ok {
		return nil, notFound("platform not found")
	}
	snapshot := s.Router.SnapshotIPLoad(platformID)
	result := make([]IPLoadEntry, 0, len(snapshot))
	for ip, count := range snapshot {
		result = append(result, IPLoadEntry{
			EgressIP:   ip.String(),
			LeaseCount: count,
		})
	}
	return result, nil
}

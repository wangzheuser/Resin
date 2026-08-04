package api

import (
	"cmp"
	"net/http"
	"slices"
	"strings"

	"github.com/Resinat/Resin/internal/service"
)

const maxLeasePageLimit = 1000

func validateAccountPath(r *http.Request) (string, error) {
	account := PathParam(r, "account")
	if strings.TrimSpace(account) == "" {
		return "", invalidArgumentError("account: must be non-empty")
	}
	return account, nil
}

func compareIPLoadEntries(sortBy string, a, b service.IPLoadEntry) int {
	switch sortBy {
	case "egress_ip":
		return strings.Compare(a.EgressIP, b.EgressIP)
	default: // lease_count
		order := cmp.Compare(a.LeaseCount, b.LeaseCount)
		if order != 0 {
			return order
		}
		return strings.Compare(a.EgressIP, b.EgressIP)
	}
}

func sortIPLoadEntries(entries []service.IPLoadEntry, sorting Sorting) {
	slices.SortStableFunc(entries, func(a, b service.IPLoadEntry) int {
		return applySortOrder(compareIPLoadEntries(sorting.SortBy, a, b), sorting.SortOrder)
	})
}

// HandleListLeases returns a handler for GET /api/v1/platforms/{id}/leases.
func HandleListLeases(cp *service.ControlPlaneService) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		platformID, ok := requireUUIDPathParam(w, r, "id", "platform_id")
		if !ok {
			return
		}

		fuzzy, ok := parseStrictBoolQuery(w, r, "fuzzy")
		if !ok {
			return
		}
		useFuzzyAccountMatch := fuzzy != nil && *fuzzy

		account := ""
		if raw := r.URL.Query().Get("account"); raw != "" {
			account = strings.TrimSpace(raw)
			if account == "" {
				writeInvalidArgument(w, "account query: must be non-empty when provided")
				return
			}
		}

		sorting, ok := parseSortingOrWriteInvalid(w, r, []string{"account", "expiry", "last_accessed"}, "expiry", "asc")
		if !ok {
			return
		}
		pg, ok := parsePaginationOrWriteInvalid(w, r)
		if !ok {
			return
		}
		if pg.Limit > maxLeasePageLimit {
			writeInvalidArgument(w, "limit: must be <= 1000 for lease listings")
			return
		}

		leases, total, err := cp.ListLeasesPage(platformID, service.LeaseListOptions{
			Account:   account,
			Fuzzy:     useFuzzyAccountMatch,
			SortBy:    sorting.SortBy,
			SortOrder: sorting.SortOrder,
			Limit:     pg.Limit,
			Offset:    pg.Offset,
		})
		if err != nil {
			writeServiceError(w, err)
			return
		}

		WriteJSON(w, http.StatusOK, PageResponse[service.LeaseResponse]{
			Items:  leases,
			Total:  total,
			Limit:  pg.Limit,
			Offset: pg.Offset,
		})
	}
}

// HandleGetLease returns a handler for GET /api/v1/platforms/{id}/leases/{account}.
func HandleGetLease(cp *service.ControlPlaneService) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		platformID, ok := requireUUIDPathParam(w, r, "id", "platform_id")
		if !ok {
			return
		}
		account, err := validateAccountPath(r)
		if err != nil {
			writeServiceError(w, err)
			return
		}
		lease, err := cp.GetLease(platformID, account)
		if err != nil {
			writeServiceError(w, err)
			return
		}
		WriteJSON(w, http.StatusOK, lease)
	}
}

// HandleDeleteLease returns a handler for DELETE /api/v1/platforms/{id}/leases/{account}.
func HandleDeleteLease(cp *service.ControlPlaneService) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		platformID, ok := requireUUIDPathParam(w, r, "id", "platform_id")
		if !ok {
			return
		}
		account, err := validateAccountPath(r)
		if err != nil {
			writeServiceError(w, err)
			return
		}
		if err := cp.DeleteLease(platformID, account); err != nil {
			writeServiceError(w, err)
			return
		}
		w.WriteHeader(http.StatusNoContent)
	}
}

// HandleDeleteAllLeases returns a handler for DELETE /api/v1/platforms/{id}/leases.
func HandleDeleteAllLeases(cp *service.ControlPlaneService) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		platformID, ok := requireUUIDPathParam(w, r, "id", "platform_id")
		if !ok {
			return
		}
		if err := cp.DeleteAllLeases(platformID); err != nil {
			writeServiceError(w, err)
			return
		}
		w.WriteHeader(http.StatusNoContent)
	}
}

// HandleIPLoad returns a handler for GET /api/v1/platforms/{id}/ip-load.
func HandleIPLoad(cp *service.ControlPlaneService) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		platformID, ok := requireUUIDPathParam(w, r, "id", "platform_id")
		if !ok {
			return
		}

		entries, err := cp.GetIPLoad(platformID)
		if err != nil {
			writeServiceError(w, err)
			return
		}

		sorting, ok := parseSortingOrWriteInvalid(w, r, []string{"egress_ip", "lease_count"}, "lease_count", "desc")
		if !ok {
			return
		}
		sortIPLoadEntries(entries, sorting)

		pg, ok := parsePaginationOrWriteInvalid(w, r)
		if !ok {
			return
		}
		WritePage(w, http.StatusOK, entries, pg)
	}
}

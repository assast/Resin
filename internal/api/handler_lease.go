package api

import (
	"cmp"
	"net/http"
	"slices"
	"strings"

	"github.com/Resinat/Resin/internal/service"
)

func validateAccountPath(r *http.Request) (string, error) {
	account := PathParam(r, "account")
	if strings.TrimSpace(account) == "" {
		return "", invalidArgumentError("account: must be non-empty")
	}
	return account, nil
}

func leaseSortKey(sortBy string, l service.LeaseResponse) string {
	switch sortBy {
	case "expiry":
		return l.Expiry
	case "last_accessed":
		return l.LastAccessed
	default:
		return l.Account
	}
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

func matchLeaseText(value, keyword string, fuzzy bool) bool {
	if fuzzy {
		return strings.Contains(strings.ToLower(value), strings.ToLower(keyword))
	}
	return value == keyword
}

func matchLeaseNode(lease service.LeaseResponse, keyword string, fuzzy bool) bool {
	return matchLeaseText(lease.NodeTag, keyword, fuzzy) || matchLeaseText(lease.NodeHash, keyword, fuzzy)
}

func parseOptionalLeaseFilter(w http.ResponseWriter, r *http.Request, name string) (string, bool) {
	raw := r.URL.Query().Get(name)
	if raw == "" {
		return "", true
	}
	value := strings.TrimSpace(raw)
	if value == "" {
		writeInvalidArgument(w, name+" query: must be non-empty when provided")
		return "", false
	}
	return value, true
}

func filterLeases(
	leases []service.LeaseResponse,
	account, node, egressIP string,
	fuzzy bool,
) []service.LeaseResponse {
	if account == "" && node == "" && egressIP == "" {
		return leases
	}

	filtered := make([]service.LeaseResponse, 0, len(leases))
	for _, lease := range leases {
		if account != "" && !matchLeaseText(lease.Account, account, fuzzy) {
			continue
		}
		if node != "" && !matchLeaseNode(lease, node, fuzzy) {
			continue
		}
		if egressIP != "" && !matchLeaseText(lease.EgressIP, egressIP, fuzzy) {
			continue
		}
		filtered = append(filtered, lease)
	}
	return filtered
}

// HandleListLeases returns a handler for GET /api/v1/platforms/{id}/leases.
func HandleListLeases(cp *service.ControlPlaneService) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		platformID, ok := requireUUIDPathParam(w, r, "id", "platform_id")
		if !ok {
			return
		}

		leases, err := cp.ListLeases(platformID)
		if err != nil {
			writeServiceError(w, err)
			return
		}

		fuzzy, ok := parseStrictBoolQuery(w, r, "fuzzy")
		if !ok {
			return
		}
		useFuzzyMatch := fuzzy != nil && *fuzzy

		account, ok := parseOptionalLeaseFilter(w, r, "account")
		if !ok {
			return
		}
		node, ok := parseOptionalLeaseFilter(w, r, "node")
		if !ok {
			return
		}
		egressIP, ok := parseOptionalLeaseFilter(w, r, "egress_ip")
		if !ok {
			return
		}
		leases = filterLeases(leases, account, node, egressIP, useFuzzyMatch)

		sorting, ok := parseSortingOrWriteInvalid(w, r, []string{"account", "expiry", "last_accessed"}, "expiry", "asc")
		if !ok {
			return
		}
		SortSlice(leases, sorting, func(l service.LeaseResponse) string {
			return leaseSortKey(sorting.SortBy, l)
		})

		pg, ok := parsePaginationOrWriteInvalid(w, r)
		if !ok {
			return
		}
		WritePage(w, http.StatusOK, leases, pg)
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

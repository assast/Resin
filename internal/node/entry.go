package node

import (
	"encoding/json"
	"net/netip"
	"regexp"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/sagernet/sing-box/adapter"
)

// SubLookupFunc resolves a subscription ID + node hash to the subscription's
// name, enabled status, and the tags for that node in that subscription.
// Returns ok=false if the subscription does not exist.
type SubLookupFunc func(subID string, hash Hash) (name string, enabled bool, tags []string, ok bool)

// NodeEntry represents a node in the global pool.
// Static fields are set at creation; dynamic fields use atomics or mutex.
type NodeEntry struct {
	// --- Static (immutable after creation) ---
	Hash       Hash
	RawOptions json.RawMessage
	CreatedAt  time.Time

	// --- Dynamic (guarded by mu) ---
	mu              sync.RWMutex
	subscriptionIDs []string
	LastError       string

	// Atomic dynamic fields for concurrent hot-path reads.
	FailureCount     atomic.Int32
	CircuitOpenSince atomic.Int64               // unix-nano; 0 = not open
	egressIP         atomic.Pointer[netip.Addr] // nil before first store
	egressRegion     atomic.Pointer[string]     // lowercase country code from probe trace; nil when unknown
	LastEgressUpdate atomic.Int64               // unix-nano of last successful egress-IP sample
	// Probe-attempt timestamps (unix-nano). These are updated regardless of
	// probe success/failure, and are used by probe schedulers.
	LastLatencyProbeAttempt          atomic.Int64
	LastAuthorityLatencyProbeAttempt atomic.Int64
	LastEgressUpdateAttempt          atomic.Int64
	LatencyTable                     *LatencyTable // per-domain latency stats; nil if not initialized

	// Outbound instance for this node.
	Outbound atomic.Pointer[adapter.Outbound]
}

// NewNodeEntry creates a NodeEntry with the given static fields.
// maxLatencyTableEntries controls the bounded size of the regular-domain LRU
// partition in the per-domain latency table.
// Pass 0 to skip latency table initialization (e.g. in tests that don't need it).
func NewNodeEntry(hash Hash, rawOptions json.RawMessage, createdAt time.Time, maxLatencyTableEntries int) *NodeEntry {
	e := &NodeEntry{
		Hash:       hash,
		RawOptions: rawOptions,
		CreatedAt:  createdAt,
	}
	if maxLatencyTableEntries > 0 {
		e.LatencyTable = NewLatencyTable(maxLatencyTableEntries)
	}
	return e
}

// SubscriptionIDs returns a copy of the subscription ID slice (thread-safe).
func (e *NodeEntry) SubscriptionIDs() []string {
	e.mu.RLock()
	defer e.mu.RUnlock()
	cp := make([]string, len(e.subscriptionIDs))
	copy(cp, e.subscriptionIDs)
	return cp
}

// AddSubscriptionID adds subID to the subscription set if not already present.
// Must be called under external synchronization (e.g. xsync.Compute).
func (e *NodeEntry) AddSubscriptionID(subID string) {
	e.mu.Lock()
	defer e.mu.Unlock()
	for _, id := range e.subscriptionIDs {
		if id == subID {
			return // idempotent
		}
	}
	e.subscriptionIDs = append(e.subscriptionIDs, subID)
}

// RemoveSubscriptionID removes subID from the subscription set.
// Returns true if the set is now empty (node should be deleted).
// Must be called under external synchronization (e.g. xsync.Compute).
func (e *NodeEntry) RemoveSubscriptionID(subID string) (empty bool) {
	e.mu.Lock()
	defer e.mu.Unlock()
	for i, id := range e.subscriptionIDs {
		if id == subID {
			e.subscriptionIDs = append(e.subscriptionIDs[:i], e.subscriptionIDs[i+1:]...)
			break
		}
	}
	return len(e.subscriptionIDs) == 0
}

// SubscriptionCount returns the number of subscriptions referencing this node.
func (e *NodeEntry) SubscriptionCount() int {
	e.mu.RLock()
	defer e.mu.RUnlock()
	return len(e.subscriptionIDs)
}

// MatchRegexs tests whether the node matches include/exclude regex filters.
//
// Candidate tags are formed as "<subscriptionName>/<tag>" from enabled subscriptions.
// Semantics:
//  1. If any candidate matches any exclude regex, the whole node fails (hard exclude).
//  2. If include is empty, the node passes when it has at least one enabled-subscription
//     candidate (and was not hard-excluded).
//  3. Otherwise the node passes when any single candidate matches ALL include regexes.
//
// When both include and exclude are empty:
//   - if subLookup is nil, it matches everything (compatibility fallback);
//   - otherwise, it matches only when at least one enabled subscription exists.
func (e *NodeEntry) MatchRegexs(include, exclude []*regexp.Regexp, subLookup SubLookupFunc) bool {
	if subLookup == nil {
		return len(include) == 0 && len(exclude) == 0
	}

	e.mu.RLock()
	subs := make([]string, len(e.subscriptionIDs))
	copy(subs, e.subscriptionIDs)
	e.mu.RUnlock()

	if len(include) == 0 && len(exclude) == 0 {
		for _, subID := range subs {
			_, enabled, _, ok := subLookup(subID, e.Hash)
			if ok && enabled {
				return true
			}
		}
		// Empty regex with lookup still requires at least one enabled subscription.
		return false
	}

	if len(subs) == 0 {
		return false
	}

	candidates := make([]string, 0, len(subs))
	for _, subID := range subs {
		name, enabled, tags, ok := subLookup(subID, e.Hash)
		if !ok || !enabled {
			continue
		}
		for _, tag := range tags {
			candidates = append(candidates, name+"/"+tag)
		}
	}
	if len(candidates) == 0 {
		return false
	}

	// Node-level hard exclude: any candidate hitting any exclude rejects the node.
	if len(exclude) > 0 {
		for _, candidate := range candidates {
			if matchesAny(candidate, exclude) {
				return false
			}
		}
	}

	if len(include) == 0 {
		return true
	}

	for _, candidate := range candidates {
		if matchesAll(candidate, include) {
			return true
		}
	}
	return false
}

// HasEnabledSubscription reports whether the node currently has at least one
// enabled subscription reference, based on subLookup.
//
// subLookup must apply the caller's definition of "subscription still holds
// this node" (for example, excluding evicted managed-node entries).
func (e *NodeEntry) HasEnabledSubscription(subLookup SubLookupFunc) bool {
	if e == nil || subLookup == nil {
		return false
	}

	e.mu.RLock()
	subs := make([]string, len(e.subscriptionIDs))
	copy(subs, e.subscriptionIDs)
	e.mu.RUnlock()

	for _, subID := range subs {
		_, enabled, _, ok := subLookup(subID, e.Hash)
		if ok && enabled {
			return true
		}
	}
	return false
}

// IsDisabledBySubscriptions reports whether the node should be treated as
// disabled: all referencing subscriptions are disabled (or missing/inapplicable
// by subLookup semantics).
func (e *NodeEntry) IsDisabledBySubscriptions(subLookup SubLookupFunc) bool {
	return !e.HasEnabledSubscription(subLookup)
}

// matchesAll returns true if s matches every regex in the list.
func matchesAll(s string, regexes []*regexp.Regexp) bool {
	for _, re := range regexes {
		if !re.MatchString(s) {
			return false
		}
	}
	return true
}

// matchesAny returns true if s matches at least one regex in the list.
func matchesAny(s string, regexes []*regexp.Regexp) bool {
	for _, re := range regexes {
		if re.MatchString(s) {
			return true
		}
	}
	return false
}

// --- Condition helpers for platform filtering ---

// IsCircuitOpen returns true if the node is currently circuit-broken.
func (e *NodeEntry) IsCircuitOpen() bool {
	return e.CircuitOpenSince.Load() != 0
}

// HasLatency returns true if the node has at least one latency record.
func (e *NodeEntry) HasLatency() bool {
	return e.LatencyTable != nil && e.LatencyTable.Size() > 0
}

// HasOutbound returns true if the node has a valid outbound instance.
func (e *NodeEntry) HasOutbound() bool {
	return e.Outbound.Load() != nil
}

// IsHealthy returns true when the node can be treated as healthy for
// routing/statistics: outbound is ready and circuit is not open.
func (e *NodeEntry) IsHealthy() bool {
	if e == nil {
		return false
	}
	return !e.IsCircuitOpen() && e.HasOutbound()
}

// GetEgressIP returns the node's egress IP, or the zero Addr if unknown.
func (e *NodeEntry) GetEgressIP() netip.Addr {
	ptr := e.egressIP.Load()
	if ptr == nil {
		return netip.Addr{}
	}
	return *ptr
}

// SetEgressIP stores the node's egress IP.
func (e *NodeEntry) SetEgressIP(ip netip.Addr) {
	e.egressIP.Store(&ip)
}

// GetEgressRegion returns the node's stored region from probe metadata,
// or empty string if unknown.
func (e *NodeEntry) GetEgressRegion() string {
	ptr := e.egressRegion.Load()
	if ptr == nil {
		return ""
	}
	return *ptr
}

// SetEgressRegion stores the node's explicit probe region.
// Empty input clears the stored value.
func (e *NodeEntry) SetEgressRegion(region string) {
	region = strings.ToLower(strings.TrimSpace(region))
	if region == "" {
		e.egressRegion.Store(nil)
		return
	}
	e.egressRegion.Store(&region)
}

// GetRegion resolves a node region using explicit probe metadata first,
// then GeoIP fallback from egress IP.
func (e *NodeEntry) GetRegion(geoLookup func(netip.Addr) string) string {
	if region := e.GetEgressRegion(); region != "" {
		return region
	}
	if geoLookup == nil {
		return ""
	}
	egressIP := e.GetEgressIP()
	if !egressIP.IsValid() {
		return ""
	}
	return geoLookup(egressIP)
}

// SetLastError sets the node's error string (thread-safe).
func (e *NodeEntry) SetLastError(msg string) {
	e.mu.Lock()
	e.LastError = msg
	e.mu.Unlock()
}

// GetLastError returns the node's error string (thread-safe).
func (e *NodeEntry) GetLastError() string {
	e.mu.RLock()
	defer e.mu.RUnlock()
	return e.LastError
}

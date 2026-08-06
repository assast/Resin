package service

import (
	"fmt"
	"sort"
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

// GenerateLeasesByNodeResult describes a node-based lease generation run.
type GenerateLeasesByNodeResult struct {
	Generated int `json:"generated"`
	NodeCount int `json:"node_count"`
}

// GenerateLeasesByEgressIPResult describes an egress-IP-based lease generation run.
type GenerateLeasesByEgressIPResult struct {
	Generated     int `json:"generated"`
	EgressIPCount int `json:"egress_ip_count"`
}

type leaseGenerationMode uint8

const (
	leaseGenerationByNode leaseGenerationMode = iota
	leaseGenerationByEgressIP
)

type leaseGenerationCandidate struct {
	hash     node.Hash
	tag      string
	egressIP string
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

// ListLeases returns all leases for a platform.
func (s *ControlPlaneService) ListLeases(platformID string) ([]LeaseResponse, error) {
	if _, ok := s.Pool.GetPlatform(platformID); !ok {
		return nil, notFound("platform not found")
	}
	var result []LeaseResponse
	s.Router.RangeLeases(platformID, func(account string, lease routing.Lease) bool {
		result = append(result, leaseToResponse(model.Lease{
			PlatformID:     platformID,
			Account:        account,
			NodeHash:       lease.NodeHash.Hex(),
			EgressIP:       lease.EgressIP.String(),
			ExpiryNs:       lease.ExpiryNs,
			LastAccessedNs: lease.LastAccessedNs,
		}, s.resolveLeaseNodeTag(lease.NodeHash)))
		return true
	})
	if result == nil {
		result = []LeaseResponse{}
	}
	return result, nil
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

// GenerateLeasesByNode creates one lease for each currently routable node.
// Nodes are numbered in the same stable tag-ascending order as the node list.
// Existing leases with generated account names are replaced; leases outside
// the current node count are deliberately left untouched.
func (s *ControlPlaneService) GenerateLeasesByNode(
	platformID, accountPrefix string,
	leaseTTL time.Duration,
) (*GenerateLeasesByNodeResult, error) {
	generated, err := s.generateLeases(platformID, accountPrefix, leaseTTL, leaseGenerationByNode)
	if err != nil {
		return nil, err
	}

	return &GenerateLeasesByNodeResult{
		Generated: generated,
		NodeCount: generated,
	}, nil
}

// GenerateLeasesByEgressIP creates one lease for each unique egress IP on the
// currently routable nodes. The first node in stable tag/hash order represents
// each IP.
func (s *ControlPlaneService) GenerateLeasesByEgressIP(
	platformID, accountPrefix string,
	leaseTTL time.Duration,
) (*GenerateLeasesByEgressIPResult, error) {
	generated, err := s.generateLeases(platformID, accountPrefix, leaseTTL, leaseGenerationByEgressIP)
	if err != nil {
		return nil, err
	}

	return &GenerateLeasesByEgressIPResult{
		Generated:     generated,
		EgressIPCount: generated,
	}, nil
}

func (s *ControlPlaneService) generateLeases(
	platformID, accountPrefix string,
	leaseTTL time.Duration,
	mode leaseGenerationMode,
) (int, error) {
	plat, ok := s.Pool.GetPlatform(platformID)
	if !ok || plat == nil {
		return 0, notFound("platform not found")
	}

	accountPrefix = strings.TrimSpace(accountPrefix)
	if accountPrefix == "" {
		return 0, invalidArg("account_prefix: must be non-empty")
	}
	if leaseTTL <= 0 {
		return 0, invalidArg("duration: must be > 0")
	}

	candidates := make([]leaseGenerationCandidate, 0, plat.View().Size())
	plat.View().Range(func(hash node.Hash) bool {
		entry, ok := s.Pool.GetEntry(hash)
		if !ok || entry == nil {
			return true
		}
		egressIP := entry.GetEgressIP()
		if !egressIP.IsValid() {
			return true
		}
		candidates = append(candidates, leaseGenerationCandidate{
			hash:     hash,
			tag:      s.resolveLeaseNodeTag(hash),
			egressIP: egressIP.String(),
		})
		return true
	})

	sort.SliceStable(candidates, func(i, j int) bool {
		if candidates[i].tag != candidates[j].tag {
			return candidates[i].tag < candidates[j].tag
		}
		return candidates[i].hash.Hex() < candidates[j].hash.Hex()
	})

	if mode == leaseGenerationByEgressIP {
		seenIPs := make(map[string]struct{}, len(candidates))
		uniqueCandidates := make([]leaseGenerationCandidate, 0, len(candidates))
		for _, candidate := range candidates {
			if _, seen := seenIPs[candidate.egressIP]; seen {
				continue
			}
			seenIPs[candidate.egressIP] = struct{}{}
			uniqueCandidates = append(uniqueCandidates, candidate)
		}
		candidates = uniqueCandidates
	}

	nowNs := time.Now().UnixNano()
	for i, candidate := range candidates {
		account := fmt.Sprintf("%s_%d", accountPrefix, i+1)
		if err := s.Router.UpsertLease(model.Lease{
			PlatformID:     platformID,
			Account:        account,
			NodeHash:       candidate.hash.Hex(),
			EgressIP:       candidate.egressIP,
			CreatedAtNs:    nowNs,
			ExpiryNs:       nowNs + leaseTTL.Nanoseconds(),
			LastAccessedNs: nowNs,
		}); err != nil {
			return 0, internal("generate leases", err)
		}
	}

	return len(candidates), nil
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

// BulkDeleteResult is the result of a bulk lease delete.
type BulkDeleteResult struct {
	Deleted  int `json:"deleted"`
	NotFound int `json:"not_found"`
}

// BulkDeleteLeases deletes leases for the given accounts on a platform.
// Missing individual leases are counted in NotFound and do not fail the call.
func (s *ControlPlaneService) BulkDeleteLeases(platformID string, accounts []string) (*BulkDeleteResult, error) {
	if _, ok := s.Pool.GetPlatform(platformID); !ok {
		return nil, notFound("platform not found")
	}

	result := &BulkDeleteResult{}
	for _, account := range accounts {
		account = strings.TrimSpace(account)
		if account == "" {
			result.NotFound++
			continue
		}
		if s.Router.DeleteLease(platformID, account) {
			result.Deleted++
		} else {
			result.NotFound++
		}
	}
	return result, nil
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

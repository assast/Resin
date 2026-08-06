package api

import (
	"net/http"
	"net/netip"
	"testing"
	"time"

	"github.com/Resinat/Resin/internal/model"
	"github.com/Resinat/Resin/internal/node"
	"github.com/Resinat/Resin/internal/subscription"
	"github.com/Resinat/Resin/internal/testutil"
)

func TestAPIContract_GenerateLeasesByNode(t *testing.T) {
	srv, cp, _ := newControlPlaneTestServer(t)
	platformID := mustCreatePlatform(t, srv, "lease-generate")

	sub := subscription.NewSubscription("sub-api-generate", "API Generate", "https://example.com/api-generate", true, false)
	cp.SubMgr.Register(sub)
	addNode := func(raw, tag, egressIP string) node.Hash {
		t.Helper()
		hash := node.HashFromRawOptions([]byte(raw))
		cp.Pool.AddNodeFromSub(hash, []byte(raw), sub.ID)
		sub.ManagedNodes().StoreNode(hash, subscription.ManagedNode{Tags: []string{tag}})
		entry, ok := cp.Pool.GetEntry(hash)
		if !ok {
			t.Fatalf("node %s not found", hash.Hex())
		}
		entry.SetEgressIP(netip.MustParseAddr(egressIP))
		entry.LatencyTable.Update("example.com", 25*time.Millisecond, 10*time.Minute)
		outbound := testutil.NewNoopOutbound()
		entry.Outbound.Store(&outbound)
		cp.Pool.RecordResult(hash, true)
		cp.Pool.NotifyNodeDirty(hash)
		return hash
	}
	hashA := addNode(`{"id":"api-generate-a"}`, "a", "203.0.113.40")
	hashB := addNode(`{"id":"api-generate-b"}`, "b", "203.0.113.41")

	rec := doJSONRequest(t, srv, http.MethodPost, "/api/v1/platforms/"+platformID+"/leases/actions/generate-by-node", map[string]any{
		"duration":       "2h",
		"account_prefix": "user",
	}, true)
	if rec.Code != http.StatusOK {
		t.Fatalf("status: got %d, want %d, body=%s", rec.Code, http.StatusOK, rec.Body.String())
	}
	body := decodeJSONMap(t, rec)
	if body["generated"] != float64(2) || body["node_count"] != float64(2) {
		t.Fatalf("result: got %v, want generated/node_count=2", body)
	}
	leaseA := cp.Router.ReadLease(model.LeaseKey{PlatformID: platformID, Account: "user_1"})
	leaseB := cp.Router.ReadLease(model.LeaseKey{PlatformID: platformID, Account: "user_2"})
	if leaseA == nil || leaseB == nil {
		t.Fatalf("generated leases missing: user_1=%+v user_2=%+v", leaseA, leaseB)
	}
	if leaseA.NodeHash != hashA.Hex() || leaseB.NodeHash != hashB.Hex() {
		t.Fatalf("generated node mapping: user_1=%q user_2=%q", leaseA.NodeHash, leaseB.NodeHash)
	}

	cases := []struct {
		name string
		body any
	}{
		{name: "missing duration", body: map[string]any{"account_prefix": "user"}},
		{name: "invalid duration", body: map[string]any{"duration": "later", "account_prefix": "user"}},
		{name: "empty prefix", body: map[string]any{"duration": "1h", "account_prefix": " "}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			rec := doJSONRequest(t, srv, http.MethodPost, "/api/v1/platforms/"+platformID+"/leases/actions/generate-by-node", tc.body, true)
			if rec.Code != http.StatusBadRequest {
				t.Fatalf("status: got %d, want %d, body=%s", rec.Code, http.StatusBadRequest, rec.Body.String())
			}
		})
	}
}

func TestAPIContract_GenerateLeasesByEgressIP(t *testing.T) {
	srv, cp, _ := newControlPlaneTestServer(t)
	platformID := mustCreatePlatform(t, srv, "lease-generate-egress-ip")

	sub := subscription.NewSubscription("sub-api-generate-ip", "API Generate IP", "https://example.com/api-generate-ip", true, false)
	cp.SubMgr.Register(sub)
	addNode := func(raw, tag, egressIP string) node.Hash {
		t.Helper()
		hash := node.HashFromRawOptions([]byte(raw))
		cp.Pool.AddNodeFromSub(hash, []byte(raw), sub.ID)
		sub.ManagedNodes().StoreNode(hash, subscription.ManagedNode{Tags: []string{tag}})
		entry, ok := cp.Pool.GetEntry(hash)
		if !ok {
			t.Fatalf("node %s not found", hash.Hex())
		}
		entry.SetEgressIP(netip.MustParseAddr(egressIP))
		entry.LatencyTable.Update("example.com", 25*time.Millisecond, 10*time.Minute)
		outbound := testutil.NewNoopOutbound()
		entry.Outbound.Store(&outbound)
		cp.Pool.RecordResult(hash, true)
		cp.Pool.NotifyNodeDirty(hash)
		return hash
	}
	hashFirst := addNode(`{"id":"api-generate-ip-first"}`, "a", "203.0.113.50")
	_ = addNode(`{"id":"api-generate-ip-shared"}`, "z", "203.0.113.50")
	hashSecond := addNode(`{"id":"api-generate-ip-second"}`, "b", "203.0.113.51")

	rec := doJSONRequest(t, srv, http.MethodPost, "/api/v1/platforms/"+platformID+"/leases/actions/generate-by-egress-ip", map[string]any{
		"duration":       "2h",
		"account_prefix": "ipuser",
	}, true)
	if rec.Code != http.StatusOK {
		t.Fatalf("status: got %d, want %d, body=%s", rec.Code, http.StatusOK, rec.Body.String())
	}
	body := decodeJSONMap(t, rec)
	if body["generated"] != float64(2) || body["egress_ip_count"] != float64(2) {
		t.Fatalf("result: got %v, want generated/egress_ip_count=2", body)
	}
	leaseFirst := cp.Router.ReadLease(model.LeaseKey{PlatformID: platformID, Account: "ipuser_1"})
	leaseSecond := cp.Router.ReadLease(model.LeaseKey{PlatformID: platformID, Account: "ipuser_2"})
	if leaseFirst == nil || leaseSecond == nil {
		t.Fatalf("generated leases missing: ipuser_1=%+v ipuser_2=%+v", leaseFirst, leaseSecond)
	}
	if leaseFirst.NodeHash != hashFirst.Hex() || leaseFirst.EgressIP != "203.0.113.50" {
		t.Fatalf("first lease: got node=%q ip=%q", leaseFirst.NodeHash, leaseFirst.EgressIP)
	}
	if leaseSecond.NodeHash != hashSecond.Hex() || leaseSecond.EgressIP != "203.0.113.51" {
		t.Fatalf("second lease: got node=%q ip=%q", leaseSecond.NodeHash, leaseSecond.EgressIP)
	}

	cases := []struct {
		name string
		body any
	}{
		{name: "missing duration", body: map[string]any{"account_prefix": "ipuser"}},
		{name: "invalid duration", body: map[string]any{"duration": "later", "account_prefix": "ipuser"}},
		{name: "empty prefix", body: map[string]any{"duration": "1h", "account_prefix": " "}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			rec := doJSONRequest(t, srv, http.MethodPost, "/api/v1/platforms/"+platformID+"/leases/actions/generate-by-egress-ip", tc.body, true)
			if rec.Code != http.StatusBadRequest {
				t.Fatalf("status: got %d, want %d, body=%s", rec.Code, http.StatusBadRequest, rec.Body.String())
			}
		})
	}
}

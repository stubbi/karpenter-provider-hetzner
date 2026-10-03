package instance

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"testing"

	"github.com/hetznercloud/hcloud-go/v2/hcloud"
	karpcp "sigs.k8s.io/karpenter/pkg/cloudprovider"

	apiv1 "github.com/paperclipinc/karpenter-provider-hetzner/pkg/apis/v1"
)

// mockActionWaiter is a fake ActionWaiter for testing.
type mockActionWaiter struct {
	waited int
	err    error
}

func (m *mockActionWaiter) WaitFor(_ context.Context, actions ...*hcloud.Action) error {
	m.waited += len(actions)
	return m.err
}

// mockServerClient is a fake ServerClient for testing.
type mockServerClient struct {
	servers          map[int64]*hcloud.Server
	nextID           int64
	deleted          []int64
	lastListSelector string
	action           *hcloud.Action
	nextActions      []*hcloud.Action
	createErr        error
	deleteErr        error
	lastOpts         hcloud.ServerCreateOpts
}

func newMockServerClient() *mockServerClient {
	return &mockServerClient{
		servers: make(map[int64]*hcloud.Server),
		nextID:  100,
	}
}

func (m *mockServerClient) Create(_ context.Context, opts hcloud.ServerCreateOpts) (hcloud.ServerCreateResult, *hcloud.Response, error) {
	m.lastOpts = opts
	if m.createErr != nil {
		return hcloud.ServerCreateResult{}, nil, m.createErr
	}
	id := m.nextID
	m.nextID++
	server := &hcloud.Server{ID: id, Name: opts.Name, Labels: opts.Labels, ServerType: opts.ServerType, Location: opts.Location}
	m.servers[id] = server
	return hcloud.ServerCreateResult{Server: server, Action: m.action, NextActions: m.nextActions}, nil, nil
}

func (m *mockServerClient) DeleteWithResult(_ context.Context, server *hcloud.Server) (*hcloud.ServerDeleteResult, *hcloud.Response, error) {
	if m.deleteErr != nil {
		return nil, nil, m.deleteErr
	}
	delete(m.servers, server.ID)
	m.deleted = append(m.deleted, server.ID)
	return &hcloud.ServerDeleteResult{}, nil, nil
}

func (m *mockServerClient) GetByID(_ context.Context, id int64) (*hcloud.Server, *hcloud.Response, error) {
	server, ok := m.servers[id]
	if !ok {
		return nil, nil, nil
	}
	return server, nil, nil
}

func (m *mockServerClient) AllWithOpts(_ context.Context, opts hcloud.ServerListOpts) ([]*hcloud.Server, error) {
	m.lastListSelector = opts.LabelSelector
	result := make([]*hcloud.Server, 0, len(m.servers))
	for _, s := range m.servers {
		result = append(result, s)
	}
	return result, nil
}

func TestCreate_LabelsApplied(t *testing.T) {
	client := newMockServerClient()
	p := NewProvider(client, "test-cluster")

	server, err := p.Create(context.Background(), CreateOpts{
		Name:       "test-node",
		ServerType: "cx11",
		Location:   "nbg1",
		Image:      &hcloud.Image{ID: 1},
		NodeClaim:  "my-claim",
		NodePool:   "my-pool",
	})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if server == nil {
		t.Fatal("expected server, got nil")
	}

	// Verify management labels.
	if server.Labels[apiv1.ServerLabelManagedBy] != apiv1.ServerValueManagedBy {
		t.Errorf("missing managed-by label, got %q", server.Labels[apiv1.ServerLabelManagedBy])
	}
	if server.Labels[apiv1.ServerLabelNodeClaim] != "my-claim" {
		t.Errorf("expected nodeclaim label 'my-claim', got %q", server.Labels[apiv1.ServerLabelNodeClaim])
	}
	if server.Labels[apiv1.ServerLabelNodePool] != "my-pool" {
		t.Errorf("expected nodepool label 'my-pool', got %q", server.Labels[apiv1.ServerLabelNodePool])
	}
	if server.Labels[apiv1.ServerLabelCluster] != "test-cluster" {
		t.Errorf("missing cluster label, got %q", server.Labels[apiv1.ServerLabelCluster])
	}
}

func TestCreate_CustomLabelsPreserved(t *testing.T) {
	client := newMockServerClient()
	p := NewProvider(client, "test-cluster")

	server, err := p.Create(context.Background(), CreateOpts{
		Name:       "test-node",
		ServerType: "cx11",
		Location:   "nbg1",
		Image:      &hcloud.Image{ID: 1},
		Labels:     map[string]string{"env": "prod"},
	})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if server.Labels["env"] != "prod" {
		t.Errorf("expected custom label env=prod, got %q", server.Labels["env"])
	}
}

// ownedServer is a server this provider (cluster "test-cluster") created.
func ownedServer(id int64, name string) *hcloud.Server {
	return &hcloud.Server{ID: id, Name: name, Labels: map[string]string{
		apiv1.ServerLabelManagedBy: apiv1.ServerValueManagedBy,
		apiv1.ServerLabelCluster:   "test-cluster",
	}}
}

func TestDelete_RemovesServer(t *testing.T) {
	client := newMockServerClient()
	client.servers[42] = ownedServer(42, "node-42")
	p := NewProvider(client, "test-cluster")

	err := p.Delete(context.Background(), "hcloud://42")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if _, ok := client.servers[42]; ok {
		t.Error("expected server to be deleted")
	}
}

func TestDelete_NotFoundReturnsNodeClaimNotFound(t *testing.T) {
	// Deleting a server that no longer exists must return a NodeClaimNotFoundError
	// (not nil). Karpenter's termination controller calls Delete each reconcile and
	// only removes the NodeClaim finalizer once Delete reports the instance is gone;
	// returning nil makes it requeue forever and leaks the NodeClaim.
	client := newMockServerClient()
	p := NewProvider(client, "test-cluster")

	err := p.Delete(context.Background(), "hcloud://999")
	if !karpcp.IsNodeClaimNotFoundError(err) {
		t.Fatalf("expected NodeClaimNotFoundError for non-existent server, got: %v", err)
	}
}

func TestDelete_RaceDeletedReturnsNodeClaimNotFound(t *testing.T) {
	// The server exists at GetByID but is already gone by DeleteWithResult (e.g. a
	// concurrent delete in the Hetzner console). Delete must still surface a
	// NodeClaimNotFoundError so the NodeClaim can be finalized.
	client := newMockServerClient()
	client.servers[55] = ownedServer(55, "node-55")
	client.deleteErr = hcloud.Error{Code: hcloud.ErrorCodeNotFound, Message: "not found"}
	p := NewProvider(client, "test-cluster")

	err := p.Delete(context.Background(), "hcloud://55")
	if !karpcp.IsNodeClaimNotFoundError(err) {
		t.Fatalf("expected NodeClaimNotFoundError on delete race, got: %v", err)
	}
}

// TestDelete_RefusesServersItDoesNotOwn: one project holds every environment and the
// token can delete anything in it. A provider ID pointing at a static worker (Terraform
// labels it cluster=<name> but not karpenter.sh/*) or at another cluster's node must
// fail loudly, not delete, and not report NodeClaimNotFound (which would drop the claim).
func TestDelete_RefusesServersItDoesNotOwn(t *testing.T) {
	for name, labels := range map[string]map[string]string{
		"static worker": {"cluster": "test-cluster", "managed-by": "terraform"},
		"other cluster": {apiv1.ServerLabelManagedBy: apiv1.ServerValueManagedBy, apiv1.ServerLabelCluster: "prod"},
	} {
		t.Run(name, func(t *testing.T) {
			client := newMockServerClient()
			client.servers[7] = &hcloud.Server{ID: 7, Name: "worker-07", Labels: labels}
			p := NewProvider(client, "test-cluster")

			err := p.Delete(context.Background(), "hcloud://7")
			if err == nil || karpcp.IsNodeClaimNotFoundError(err) {
				t.Fatalf("expected a plain refusal error, got: %v", err)
			}
			if _, ok := client.servers[7]; !ok || len(client.deleted) != 0 {
				t.Error("server was deleted")
			}
		})
	}
}

func TestGet_Found(t *testing.T) {
	client := newMockServerClient()
	client.servers[77] = &hcloud.Server{ID: 77, Name: "my-node"}
	p := NewProvider(client, "test-cluster")

	server, err := p.Get(context.Background(), "hcloud://77")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if server == nil {
		t.Fatal("expected server, got nil")
	}
	if server.ID != 77 {
		t.Errorf("expected ID=77, got %d", server.ID)
	}
}

func TestGet_NotFound(t *testing.T) {
	client := newMockServerClient()
	p := NewProvider(client, "test-cluster")

	server, err := p.Get(context.Background(), "hcloud://999")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if server != nil {
		t.Errorf("expected nil server for missing ID, got %+v", server)
	}
}

func TestList(t *testing.T) {
	client := newMockServerClient()
	client.servers[1] = &hcloud.Server{ID: 1, Name: "node-1"}
	client.servers[2] = &hcloud.Server{ID: 2, Name: "node-2"}
	p := NewProvider(client, "test-cluster")

	servers, err := p.List(context.Background())
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(servers) != 2 {
		t.Errorf("expected 2 servers, got %d", len(servers))
	}
}

func TestParseProviderID_Valid(t *testing.T) {
	cases := []struct {
		input    string
		expected int64
	}{
		{"hcloud://123", 123},
		{"hcloud://999999", 999999},
		{"hcloud://1", 1},
	}
	for _, tc := range cases {
		id, err := ParseProviderID(tc.input)
		if err != nil {
			t.Errorf("ParseProviderID(%q) unexpected error: %v", tc.input, err)
			continue
		}
		if id != tc.expected {
			t.Errorf("ParseProviderID(%q) = %d, want %d", tc.input, id, tc.expected)
		}
	}
}

func TestParseProviderID_Invalid(t *testing.T) {
	cases := []string{
		"123",
		"aws://123",
		"hcloud://abc",
		"hcloud://",
		"",
	}
	for _, tc := range cases {
		_, err := ParseProviderID(tc)
		if err == nil {
			t.Errorf("ParseProviderID(%q) expected error, got nil", tc)
		}
	}
}

func TestFormatProviderID(t *testing.T) {
	cases := []struct {
		id       int64
		expected string
	}{
		{123, "hcloud://123"},
		{1, "hcloud://1"},
		{999999, "hcloud://999999"},
	}
	for _, tc := range cases {
		got := FormatProviderID(tc.id)
		if got != tc.expected {
			t.Errorf("FormatProviderID(%d) = %q, want %q", tc.id, got, tc.expected)
		}
	}
}

func TestCreate_ClusterLabelApplied(t *testing.T) {
	client := newMockServerClient()
	p := NewProvider(client, "test-cluster")
	server, err := p.Create(context.Background(), CreateOpts{
		Name: "n", ServerType: "cx22", Location: "nbg1", Image: &hcloud.Image{ID: 1},
	})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if server.Labels[apiv1.ServerLabelCluster] != "test-cluster" {
		t.Errorf("expected cluster label, got %q", server.Labels[apiv1.ServerLabelCluster])
	}
}

func TestList_ScopesByCluster(t *testing.T) {
	client := newMockServerClient()
	p := NewProvider(client, "test-cluster")
	if _, err := p.List(context.Background()); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !strings.Contains(client.lastListSelector, apiv1.ServerLabelCluster+"=test-cluster") {
		t.Errorf("List selector %q does not scope by cluster", client.lastListSelector)
	}
	if !strings.Contains(client.lastListSelector, apiv1.ServerLabelManagedBy+"="+apiv1.ServerValueManagedBy) {
		t.Errorf("List selector %q missing managed-by", client.lastListSelector)
	}
}

func TestCreate_WaitsForActionsAndSetsPublicNet(t *testing.T) {
	client := newMockServerClient()
	client.action = &hcloud.Action{ID: 1}
	waiter := &mockActionWaiter{}
	p := NewProviderWithWaiter(client, "test-cluster", waiter)

	_, err := p.Create(context.Background(), CreateOpts{
		Name: "n", ServerType: "cx22", Location: "nbg1", Image: &hcloud.Image{ID: 1},
		EnablePublicIPv4: false, EnablePublicIPv6: true,
	})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if waiter.waited == 0 {
		t.Error("expected the action waiter to be called")
	}
	if client.lastOpts.PublicNet == nil || client.lastOpts.PublicNet.EnableIPv4 {
		t.Error("expected public IPv4 to be disabled in create opts")
	}
	if !client.lastOpts.PublicNet.EnableIPv6 {
		t.Error("expected public IPv6 to default to enabled")
	}
}

func TestErrorCode(t *testing.T) {
	quota := hcloud.Error{Code: hcloud.ErrorCodeResourceLimitExceeded, Message: "shared core limit exceeded"}
	for want, err := range map[string]error{
		"resource_limit_exceeded": fmt.Errorf("creating server: %w", MapCreateError(quota)), // through the ICE wrapper
		"uniqueness_error":        fmt.Errorf("server %q: %w", "x", hcloud.Error{Code: hcloud.ErrorCodeUniquenessError}),
		"other":                   errors.New("boom"),
	} {
		if got := errorCode(err); got != want {
			t.Errorf("errorCode(%v) = %q, want %q", err, got, want)
		}
	}
}

func TestCreate_MapsCapacityError(t *testing.T) {
	client := newMockServerClient()
	client.createErr = hcloud.Error{Code: hcloud.ErrorCodeResourceUnavailable}
	p := NewProvider(client, "test-cluster")
	_, err := p.Create(context.Background(), CreateOpts{Name: "n", ServerType: "cx22", Location: "nbg1", Image: &hcloud.Image{ID: 1}})
	if !karpcp.IsInsufficientCapacityError(err) {
		t.Errorf("expected InsufficientCapacityError, got %v", err)
	}
}

func TestCreate_WaiterErrorIsWrapped(t *testing.T) {
	client := newMockServerClient()
	client.action = &hcloud.Action{ID: 1}
	waiter := &mockActionWaiter{err: fmt.Errorf("action failed")}
	p := NewProviderWithWaiter(client, "test-cluster", waiter)
	_, err := p.Create(context.Background(), CreateOpts{Name: "n", ServerType: "cx22", Location: "nbg1", Image: &hcloud.Image{ID: 1}})
	if err == nil || !strings.Contains(err.Error(), "waiting for server") {
		t.Errorf("expected wrapped wait error, got %v", err)
	}
}

func TestCreate_WaitsForNextActions(t *testing.T) {
	client := newMockServerClient()
	client.action = nil
	client.nextActions = []*hcloud.Action{{ID: 2}, {ID: 3}}
	waiter := &mockActionWaiter{}
	p := NewProviderWithWaiter(client, "test-cluster", waiter)
	if _, err := p.Create(context.Background(), CreateOpts{Name: "n", ServerType: "cx22", Location: "nbg1", Image: &hcloud.Image{ID: 1}}); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if waiter.waited != 2 {
		t.Errorf("expected 2 next-actions waited, got %d", waiter.waited)
	}
}

// ---------------------------------------------------------------------------
// mockPlacementGroupClient and placement group tests
// ---------------------------------------------------------------------------

type mockPlacementGroupClient struct {
	groups    []*hcloud.PlacementGroup
	nextID    int64
	createErr error
	// recorded arguments for assertions
	lastListOpts   hcloud.PlacementGroupListOpts
	lastCreateOpts hcloud.PlacementGroupCreateOpts
	createCalls    int
}

func newMockPlacementGroupClient() *mockPlacementGroupClient {
	return &mockPlacementGroupClient{nextID: 200}
}

func (m *mockPlacementGroupClient) AllWithOpts(_ context.Context, opts hcloud.PlacementGroupListOpts) ([]*hcloud.PlacementGroup, error) {
	m.lastListOpts = opts
	var result []*hcloud.PlacementGroup
	for _, pg := range m.groups {
		if opts.Name != "" && pg.Name != opts.Name {
			continue
		}
		if opts.Type != "" && pg.Type != opts.Type {
			continue
		}
		result = append(result, pg)
	}
	return result, nil
}

func (m *mockPlacementGroupClient) Create(_ context.Context, opts hcloud.PlacementGroupCreateOpts) (hcloud.PlacementGroupCreateResult, *hcloud.Response, error) {
	m.lastCreateOpts = opts
	m.createCalls++
	if m.createErr != nil {
		return hcloud.PlacementGroupCreateResult{}, nil, m.createErr
	}
	id := m.nextID
	m.nextID++
	pg := &hcloud.PlacementGroup{ID: id, Name: opts.Name, Type: opts.Type}
	m.groups = append(m.groups, pg)
	return hcloud.PlacementGroupCreateResult{PlacementGroup: pg}, nil, nil
}

// TestCreate_SpreadStrategy_CreatesPG verifies that strategy "spread" (default)
// causes a placement group to be created and assigned to the server.
func TestCreate_SpreadStrategy_CreatesPG(t *testing.T) {
	sc := newMockServerClient()
	pgc := newMockPlacementGroupClient()
	p := NewProviderWithPlacementGroups(sc, pgc, "test-cluster", nil)

	_, err := p.Create(context.Background(), CreateOpts{
		Name:                   "n",
		ServerType:             "cx22",
		Location:               "nbg1",
		Image:                  &hcloud.Image{ID: 1},
		NodePool:               "my-pool",
		PlacementGroupStrategy: "spread",
	})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	// A placement group should have been created.
	if pgc.createCalls != 1 {
		t.Errorf("expected 1 PG create call, got %d", pgc.createCalls)
	}
	if pgc.lastCreateOpts.Name != "karpenter-test-cluster-my-pool" {
		t.Errorf("expected PG name %q, got %q", "karpenter-test-cluster-my-pool", pgc.lastCreateOpts.Name)
	}
	if pgc.lastCreateOpts.Type != hcloud.PlacementGroupTypeSpread {
		t.Errorf("expected spread type, got %q", pgc.lastCreateOpts.Type)
	}
	// The server create opts must include the placement group.
	if sc.lastOpts.PlacementGroup == nil {
		t.Error("expected PlacementGroup to be set on server create opts")
	}
}

// TestCreate_SpreadStrategy_EmptyStrategy_CreatesPG verifies that an empty
// strategy (the kubebuilder default "spread") also creates a placement group.
func TestCreate_SpreadStrategy_EmptyStrategy_CreatesPG(t *testing.T) {
	sc := newMockServerClient()
	pgc := newMockPlacementGroupClient()
	p := NewProviderWithPlacementGroups(sc, pgc, "test-cluster", nil)

	_, err := p.Create(context.Background(), CreateOpts{
		Name:       "n",
		ServerType: "cx22",
		Location:   "nbg1",
		Image:      &hcloud.Image{ID: 1},
		NodePool:   "pool-a",
		// PlacementGroupStrategy intentionally left empty -> treated as spread
	})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if sc.lastOpts.PlacementGroup == nil {
		t.Error("expected PlacementGroup to be set on server create opts when strategy is empty")
	}
}

// TestCreate_NoneStrategy_NoPG verifies that strategy "none" does NOT create or
// assign a placement group.
func TestCreate_NoneStrategy_NoPG(t *testing.T) {
	sc := newMockServerClient()
	pgc := newMockPlacementGroupClient()
	p := NewProviderWithPlacementGroups(sc, pgc, "test-cluster", nil)

	_, err := p.Create(context.Background(), CreateOpts{
		Name:                   "n",
		ServerType:             "cx22",
		Location:               "nbg1",
		Image:                  &hcloud.Image{ID: 1},
		NodePool:               "my-pool",
		PlacementGroupStrategy: "none",
	})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if pgc.createCalls != 0 {
		t.Errorf("expected 0 PG create calls for strategy=none, got %d", pgc.createCalls)
	}
	if sc.lastOpts.PlacementGroup != nil {
		t.Error("expected no PlacementGroup on server create opts when strategy=none")
	}
}

// TestCreate_SpreadStrategy_ReusesPG verifies that when a placement group with
// the expected name already exists, Create reuses it without calling create.
func TestCreate_SpreadStrategy_ReusesPG(t *testing.T) {
	sc := newMockServerClient()
	pgc := newMockPlacementGroupClient()
	// Pre-seed an existing placement group with the expected name.
	existingID := int64(999)
	pgc.groups = []*hcloud.PlacementGroup{
		{ID: existingID, Name: "karpenter-test-cluster-my-pool", Type: hcloud.PlacementGroupTypeSpread},
	}
	p := NewProviderWithPlacementGroups(sc, pgc, "test-cluster", nil)

	_, err := p.Create(context.Background(), CreateOpts{
		Name:                   "n",
		ServerType:             "cx22",
		Location:               "nbg1",
		Image:                  &hcloud.Image{ID: 1},
		NodePool:               "my-pool",
		PlacementGroupStrategy: "spread",
	})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	// Should NOT have called create.
	if pgc.createCalls != 0 {
		t.Errorf("expected 0 PG create calls (reuse), got %d", pgc.createCalls)
	}
	// Should use the existing PG's ID.
	if sc.lastOpts.PlacementGroup == nil || sc.lastOpts.PlacementGroup.ID != existingID {
		t.Errorf("expected existing PG ID %d, got %v", existingID, sc.lastOpts.PlacementGroup)
	}
}

// TestCreate_SpreadStrategy_EmptyNodePool verifies the fallback PG name when
// NodePool is empty.
func TestCreate_SpreadStrategy_EmptyNodePool(t *testing.T) {
	sc := newMockServerClient()
	pgc := newMockPlacementGroupClient()
	p := NewProviderWithPlacementGroups(sc, pgc, "test-cluster", nil)

	_, err := p.Create(context.Background(), CreateOpts{
		Name:                   "n",
		ServerType:             "cx22",
		Location:               "nbg1",
		Image:                  &hcloud.Image{ID: 1},
		NodePool:               "", // empty
		PlacementGroupStrategy: "spread",
	})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if pgc.lastCreateOpts.Name != "karpenter-test-cluster" {
		t.Errorf("expected PG name %q for empty NodePool, got %q", "karpenter-test-cluster", pgc.lastCreateOpts.Name)
	}
}

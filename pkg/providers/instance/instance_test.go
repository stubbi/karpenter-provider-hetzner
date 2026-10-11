package instance

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"sync"
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
	mu               sync.Mutex
	servers          map[int64]*hcloud.Server
	nextID           int64
	deleted          []int64
	lastListSelector string
	action           *hcloud.Action
	nextActions      []*hcloud.Action
	createErr        error
	deleteErr        error
	lastOpts         hcloud.ServerCreateOpts
	createCalls      int
	// pgs, when set, makes Create enforce spread-group capacity the way hcloud
	// does: a create into a full group fails with placement_error.
	pgs *mockPlacementGroupClient
}

func newMockServerClient() *mockServerClient {
	return &mockServerClient{
		servers: make(map[int64]*hcloud.Server),
		nextID:  100,
	}
}

func (m *mockServerClient) Create(_ context.Context, opts hcloud.ServerCreateOpts) (hcloud.ServerCreateResult, *hcloud.Response, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.lastOpts = opts
	m.createCalls++
	if m.createErr != nil {
		return hcloud.ServerCreateResult{}, nil, m.createErr
	}
	id := m.nextID
	if m.pgs != nil && opts.PlacementGroup != nil {
		if err := m.pgs.addServer(opts.PlacementGroup.ID, id); err != nil {
			return hcloud.ServerCreateResult{}, nil, err
		}
	}
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
	mu        sync.Mutex
	listCalls int
	getCalls  int
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
	m.mu.Lock()
	defer m.mu.Unlock()
	m.listCalls++
	m.lastListOpts = opts
	var result []*hcloud.PlacementGroup
	for _, pg := range m.groups {
		if opts.Name != "" && pg.Name != opts.Name {
			continue
		}
		if opts.Type != "" && pg.Type != opts.Type {
			continue
		}
		if !matchesSelector(pg.Labels, opts.LabelSelector) {
			continue
		}
		cp := *pg
		cp.Servers = append([]int64(nil), pg.Servers...)
		result = append(result, &cp)
	}
	return result, nil
}

// matchesSelector evaluates an equality-only hcloud label selector ("a=b,c=d").
func matchesSelector(labels map[string]string, selector string) bool {
	if selector == "" {
		return true
	}
	for _, term := range strings.Split(selector, ",") {
		k, v, _ := strings.Cut(term, "=")
		if got, ok := labels[k]; !ok || got != v {
			return false
		}
	}
	return true
}

func (m *mockPlacementGroupClient) GetByID(_ context.Context, id int64) (*hcloud.PlacementGroup, *hcloud.Response, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.getCalls++
	for _, pg := range m.groups {
		if pg.ID == id {
			cp := *pg
			cp.Servers = append([]int64(nil), pg.Servers...)
			return &cp, nil, nil
		}
	}
	return nil, nil, nil
}

// addServer records a server in a group, refusing it once the group holds
// spreadGroupMaxServers, as hcloud does.
func (m *mockPlacementGroupClient) addServer(groupID, serverID int64) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	for _, pg := range m.groups {
		if pg.ID != groupID {
			continue
		}
		if len(pg.Servers) >= spreadGroupMaxServers {
			return hcloud.Error{Code: hcloud.ErrorCodePlacementError, Message: "placement group is full"}
		}
		pg.Servers = append(pg.Servers, serverID)
		return nil
	}
	return hcloud.Error{Code: hcloud.ErrorCodeNotFound, Message: "placement group not found"}
}

func (m *mockPlacementGroupClient) Create(_ context.Context, opts hcloud.PlacementGroupCreateOpts) (hcloud.PlacementGroupCreateResult, *hcloud.Response, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.lastCreateOpts = opts
	m.createCalls++
	if m.createErr != nil {
		return hcloud.PlacementGroupCreateResult{}, nil, m.createErr
	}
	for _, pg := range m.groups {
		if pg.Name == opts.Name {
			return hcloud.PlacementGroupCreateResult{}, nil, hcloud.Error{Code: hcloud.ErrorCodeUniquenessError, Message: "name is already used"}
		}
	}
	id := m.nextID
	m.nextID++
	pg := &hcloud.PlacementGroup{ID: id, Name: opts.Name, Type: opts.Type, Labels: opts.Labels}
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

// TestCreate_SpreadStrategy_ConcurrentCreatesShareOneGroup is the regression for
// the get-or-create race (#69) and the per-create list call (#70): a burst of
// concurrent creates for one pool must resolve the group exactly once.
func TestCreate_SpreadStrategy_ConcurrentCreatesShareOneGroup(t *testing.T) {
	sc := newMockServerClient()
	pgc := newMockPlacementGroupClient()
	p := NewProviderWithPlacementGroups(sc, pgc, "test-cluster", nil)

	var wg sync.WaitGroup
	for i := 0; i < spreadGroupMaxServers; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			if _, err := p.Create(context.Background(), CreateOpts{
				Name: fmt.Sprintf("n-%d", i), ServerType: "cx22", Location: "nbg1",
				Image: &hcloud.Image{ID: 1}, NodePool: "burst", PlacementGroupStrategy: "spread",
			}); err != nil {
				t.Errorf("create %d: %v", i, err)
			}
		}(i)
	}
	wg.Wait()

	if pgc.createCalls != 1 {
		t.Errorf("concurrent creates made %d placement groups, want 1", pgc.createCalls)
	}
	// One refresh of the pool: the legacy by-name lookup plus the labelled list.
	if pgc.listCalls != 2 {
		t.Errorf("concurrent creates listed placement groups %d times, want 2 (one refresh)", pgc.listCalls)
	}
	if len(pgc.groups) != 1 || sc.lastOpts.PlacementGroup.ID != pgc.groups[0].ID {
		t.Errorf("servers did not all land in the one group")
	}
}

// TestCreate_SpreadStrategy_ForgetsGroupOnNotFound checks a group deleted out of
// band is resolved afresh instead of failing every create on a stale cached ID.
func TestCreate_SpreadStrategy_ForgetsGroupOnNotFound(t *testing.T) {
	sc := newMockServerClient()
	pgc := newMockPlacementGroupClient()
	p := NewProviderWithPlacementGroups(sc, pgc, "test-cluster", nil)
	opts := CreateOpts{Name: "n", ServerType: "cx22", Location: "nbg1", Image: &hcloud.Image{ID: 1}, NodePool: "pool"}

	if _, err := p.Create(context.Background(), opts); err != nil {
		t.Fatal(err)
	}
	// The group disappears out of band; the next create fails on the stale ID.
	pgc.groups = nil
	sc.createErr = hcloud.Error{Code: hcloud.ErrorCodeNotFound, Message: "placement group not found"}
	if _, err := p.Create(context.Background(), opts); err == nil {
		t.Fatal("expected the create on a deleted group to fail")
	}
	sc.createErr = nil
	if _, err := p.Create(context.Background(), opts); err != nil {
		t.Fatal(err)
	}
	if pgc.createCalls != 2 {
		t.Errorf("placement group created %d times, want 2 (once, then again after not_found)", pgc.createCalls)
	}
	if got := sc.lastOpts.PlacementGroup.ID; got != pgc.groups[0].ID {
		t.Errorf("create used group %d, want the recreated group %d", got, pgc.groups[0].ID)
	}
}

func spreadOpts(name, pool string) CreateOpts {
	return CreateOpts{Name: name, ServerType: "cx22", Location: "nbg1", Image: &hcloud.Image{ID: 1}, NodePool: pool, PlacementGroupStrategy: "spread"}
}

// groupSizes returns the member count of every group, keyed by name.
func groupSizes(pgc *mockPlacementGroupClient) map[string]int {
	out := map[string]int{}
	for _, g := range pgc.groups {
		out[g.Name] = len(g.Servers)
	}
	return out
}

// TestCreate_SpreadStrategy_RollsOverToIndexedGroups is the #69 ceiling: a pool
// larger than one spread group used to stop scaling at the group's cap.
func TestCreate_SpreadStrategy_RollsOverToIndexedGroups(t *testing.T) {
	sc := newMockServerClient()
	pgc := newMockPlacementGroupClient()
	sc.pgs = pgc
	p := NewProviderWithPlacementGroups(sc, pgc, "c", nil)

	for i := 0; i < 25; i++ {
		if _, err := p.Create(context.Background(), spreadOpts(fmt.Sprintf("n-%d", i), "pool")); err != nil {
			t.Fatalf("create %d: %v", i, err)
		}
	}
	want := map[string]int{"karpenter-c-pool": 10, "karpenter-c-pool-1": 10, "karpenter-c-pool-2": 5}
	if got := groupSizes(pgc); fmt.Sprint(got) != fmt.Sprint(want) {
		t.Errorf("group sizes %v, want %v", got, want)
	}
	for _, g := range pgc.groups {
		if g.Labels[LabelPlacementGroupIndex] == "" || g.Labels[apiv1.ServerLabelNodePool] != "pool" ||
			g.Labels[apiv1.ServerLabelCluster] != "c" {
			t.Errorf("group %s is not labelled for its pool: %v", g.Name, g.Labels)
		}
	}
}

// TestCreate_SpreadStrategy_PicksLeastFullGroup checks freed slots are reused,
// least-full first, instead of piling into one group or creating another.
func TestCreate_SpreadStrategy_PicksLeastFullGroup(t *testing.T) {
	sc := newMockServerClient()
	pgc := newMockPlacementGroupClient()
	sc.pgs = pgc
	pgc.groups = []*hcloud.PlacementGroup{
		{ID: 1, Name: "karpenter-c-pool", Type: hcloud.PlacementGroupTypeSpread, Servers: []int64{1, 2, 3, 4, 5, 6, 7, 8, 9, 10}},
		{ID: 2, Name: "karpenter-c-pool-1", Type: hcloud.PlacementGroupTypeSpread, Servers: []int64{11, 12, 13, 14, 15, 16, 17},
			Labels: map[string]string{apiv1.ServerLabelManagedBy: "karpenter", apiv1.ServerLabelCluster: "c", apiv1.ServerLabelNodePool: "pool", LabelPlacementGroupIndex: "1"}},
		{ID: 3, Name: "karpenter-c-pool-2", Type: hcloud.PlacementGroupTypeSpread, Servers: []int64{18, 19},
			Labels: map[string]string{apiv1.ServerLabelManagedBy: "karpenter", apiv1.ServerLabelCluster: "c", apiv1.ServerLabelNodePool: "pool", LabelPlacementGroupIndex: "2"}},
	}
	p := NewProviderWithPlacementGroups(sc, pgc, "c", nil)

	if _, err := p.Create(context.Background(), spreadOpts("n", "pool")); err != nil {
		t.Fatal(err)
	}
	if got := sc.lastOpts.PlacementGroup.ID; got != 3 {
		t.Errorf("server went into group %d, want the least-full group 3", got)
	}
	if pgc.createCalls != 0 {
		t.Errorf("created %d groups while existing ones had room", pgc.createCalls)
	}
}

// TestCreate_SpreadStrategy_IgnoresOtherPoolsGroups checks the node pool label
// keeps pool "a"'s overflow groups and pool "a-1"'s groups apart, although
// karpenter-c-a-1 would be a valid name for both.
func TestCreate_SpreadStrategy_IgnoresOtherPoolsGroups(t *testing.T) {
	sc := newMockServerClient()
	pgc := newMockPlacementGroupClient()
	sc.pgs = pgc
	pgc.groups = []*hcloud.PlacementGroup{
		{ID: 7, Name: "karpenter-c-a-1", Type: hcloud.PlacementGroupTypeSpread,
			Labels: map[string]string{apiv1.ServerLabelManagedBy: "karpenter", apiv1.ServerLabelCluster: "c", apiv1.ServerLabelNodePool: "a-1", LabelPlacementGroupIndex: "0"}},
	}
	p := NewProviderWithPlacementGroups(sc, pgc, "c", nil)

	if _, err := p.Create(context.Background(), spreadOpts("n", "a")); err != nil {
		t.Fatal(err)
	}
	if got := sc.lastOpts.PlacementGroup.ID; got == 7 {
		t.Error("pool a placed a server in pool a-1's group")
	}
}

// TestCreate_SpreadStrategy_OverflowOnCreateError is the race the owner asked to
// see as a test: the chosen group fills between the count and the create (another
// controller, or a create the cache did not see), hcloud answers placement_error,
// and the provider must move to another group rather than fail or retry the same.
func TestCreate_SpreadStrategy_OverflowOnCreateError(t *testing.T) {
	sc := newMockServerClient()
	pgc := newMockPlacementGroupClient()
	sc.pgs = pgc
	p := NewProviderWithPlacementGroups(sc, pgc, "c", nil)

	if _, err := p.Create(context.Background(), spreadOpts("first", "pool")); err != nil {
		t.Fatal(err)
	}
	// The group fills behind the cache's back: it believes one member.
	for id := int64(1000); len(pgc.groups[0].Servers) < spreadGroupMaxServers; id++ {
		pgc.groups[0].Servers = append(pgc.groups[0].Servers, id)
	}

	server, err := p.Create(context.Background(), spreadOpts("second", "pool"))
	if err != nil {
		t.Fatalf("create after the group filled: %v", err)
	}
	if server == nil || sc.lastOpts.PlacementGroup.ID == pgc.groups[0].ID {
		t.Fatalf("server landed in the full group")
	}
	if len(pgc.groups) != 2 || len(pgc.groups[1].Servers) != 1 {
		t.Errorf("want the server in a new overflow group, groups: %v", groupSizes(pgc))
	}
	if sc.createCalls != 3 {
		t.Errorf("server create called %d times, want 3 (first, refused, retried)", sc.createCalls)
	}

	// The full group stays marked full: the next create goes straight to group 1.
	before := sc.createCalls
	if _, err := p.Create(context.Background(), spreadOpts("third", "pool")); err != nil {
		t.Fatal(err)
	}
	if sc.createCalls-before != 1 || sc.lastOpts.PlacementGroup.ID != pgc.groups[1].ID {
		t.Errorf("third create was not sent straight to the overflow group")
	}
}

// TestCreate_SpreadStrategy_OverflowRetriesOnlyOnce bounds the retry: a second
// placement_error is returned, not looped on.
func TestCreate_SpreadStrategy_OverflowRetriesOnlyOnce(t *testing.T) {
	sc := newMockServerClient()
	pgc := newMockPlacementGroupClient()
	p := NewProviderWithPlacementGroups(sc, pgc, "c", nil)
	sc.createErr = hcloud.Error{Code: hcloud.ErrorCodePlacementError, Message: "placement failed"}

	if _, err := p.Create(context.Background(), spreadOpts("n", "pool")); err == nil {
		t.Fatal("expected the second placement_error to be returned")
	}
	if sc.createCalls != 2 {
		t.Errorf("server create called %d times, want 2", sc.createCalls)
	}
}

// TestCreate_ExplicitPlacementGroupIDs checks operator-managed groups are used
// instead of provider-managed ones, least-full first, and never added to.
func TestCreate_ExplicitPlacementGroupIDs(t *testing.T) {
	sc := newMockServerClient()
	pgc := newMockPlacementGroupClient()
	sc.pgs = pgc
	pgc.groups = []*hcloud.PlacementGroup{
		{ID: 41, Name: "tf-a", Type: hcloud.PlacementGroupTypeSpread, Servers: []int64{1, 2, 3}},
		{ID: 42, Name: "tf-b", Type: hcloud.PlacementGroupTypeSpread, Servers: []int64{4}},
	}
	p := NewProviderWithPlacementGroups(sc, pgc, "c", nil)
	opts := spreadOpts("n", "pool")
	opts.PlacementGroupIDs = []int64{41, 42}

	if _, err := p.Create(context.Background(), opts); err != nil {
		t.Fatal(err)
	}
	if got := sc.lastOpts.PlacementGroup.ID; got != 42 {
		t.Errorf("server went into group %d, want the least-full explicit group 42", got)
	}

	// Fill both; the provider must refuse rather than create a group of its own.
	for _, g := range pgc.groups {
		for id := int64(100 * g.ID); len(g.Servers) < spreadGroupMaxServers; id++ {
			g.Servers = append(g.Servers, id)
		}
	}
	p.forgetPlacementGroups(pgPoolKey(opts, ""))
	_, err := p.Create(context.Background(), opts)
	if !errors.Is(err, ErrPlacementGroupsFull) {
		t.Errorf("full explicit groups: err = %v, want ErrPlacementGroupsFull", err)
	}
	if pgc.createCalls != 0 {
		t.Errorf("provider created %d groups although spec.placementGroupIDs is set", pgc.createCalls)
	}
}

// TestCreate_ExplicitPlacementGroupIDs_WithStrategyNone checks explicit groups
// apply even when the strategy is none (admission rejects the combination, but
// the provider must not silently drop the operator's groups if it gets through).
func TestCreate_ExplicitPlacementGroupIDs_WithStrategyNone(t *testing.T) {
	sc := newMockServerClient()
	pgc := newMockPlacementGroupClient()
	pgc.groups = []*hcloud.PlacementGroup{{ID: 41, Name: "tf-a", Type: hcloud.PlacementGroupTypeSpread}}
	p := NewProviderWithPlacementGroups(sc, pgc, "c", nil)
	opts := spreadOpts("n", "pool")
	opts.PlacementGroupStrategy = "none"
	opts.PlacementGroupIDs = []int64{41}

	if _, err := p.Create(context.Background(), opts); err != nil {
		t.Fatal(err)
	}
	if sc.lastOpts.PlacementGroup == nil || sc.lastOpts.PlacementGroup.ID != 41 {
		t.Errorf("explicit group not applied: %v", sc.lastOpts.PlacementGroup)
	}
}

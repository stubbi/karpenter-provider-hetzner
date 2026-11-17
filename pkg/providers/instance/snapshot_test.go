package instance

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"

	"github.com/hetznercloud/hcloud-go/v2/hcloud"

	apiv1 "github.com/paperclipinc/karpenter-provider-hetzner/pkg/apis/v1"
)

// countingServerClient counts list and get requests, can fail lists, and can hold a
// list open until released, so tests can assert how many hcloud requests drift checks
// cost.
type countingServerClient struct {
	mu        sync.Mutex
	servers   map[int64]*hcloud.Server
	listCalls int
	getCalls  int
	listErr   error
	// listGate, when set, blocks every list until it is closed.
	listGate chan struct{}
}

func newCountingServerClient(servers ...*hcloud.Server) *countingServerClient {
	c := &countingServerClient{servers: map[int64]*hcloud.Server{}}
	for _, s := range servers {
		c.servers[s.ID] = s
	}
	return c
}

func (c *countingServerClient) Create(context.Context, hcloud.ServerCreateOpts) (hcloud.ServerCreateResult, *hcloud.Response, error) {
	return hcloud.ServerCreateResult{}, nil, errors.New("not used")
}

func (c *countingServerClient) DeleteWithResult(context.Context, *hcloud.Server) (*hcloud.ServerDeleteResult, *hcloud.Response, error) {
	return nil, nil, errors.New("not used")
}

func (c *countingServerClient) GetByID(_ context.Context, id int64) (*hcloud.Server, *hcloud.Response, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.getCalls++
	return c.servers[id], nil, nil
}

func (c *countingServerClient) AllWithOpts(ctx context.Context, opts hcloud.ServerListOpts) ([]*hcloud.Server, error) {
	c.mu.Lock()
	c.listCalls++
	gate, err := c.listGate, c.listErr
	c.mu.Unlock()
	if gate != nil {
		select {
		case <-gate:
		case <-ctx.Done():
			return nil, ctx.Err()
		}
	}
	if err != nil {
		return nil, err
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	out := make([]*hcloud.Server, 0, len(c.servers))
	for _, s := range c.servers {
		if matchesSelector(s.Labels, opts.LabelSelector) {
			out = append(out, s)
		}
	}
	return out, nil
}

func (c *countingServerClient) counts() (list, get int) {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.listCalls, c.getCalls
}

func (c *countingServerClient) set(s *hcloud.Server) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.servers[s.ID] = s
}

func (c *countingServerClient) setListErr(err error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.listErr = err
}

type fakeClock struct {
	mu  sync.Mutex
	now time.Time
}

func (f *fakeClock) Now() time.Time {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.now
}

func (f *fakeClock) Advance(d time.Duration) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.now = f.now.Add(d)
}

func managedServer(id int64, networkID int64) *hcloud.Server {
	return &hcloud.Server{
		ID: id,
		Labels: map[string]string{
			apiv1.ServerLabelManagedBy: apiv1.ServerValueManagedBy,
			apiv1.ServerLabelCluster:   "test-cluster",
		},
		PrivateNet: []hcloud.ServerPrivateNet{{Network: &hcloud.Network{ID: networkID}}},
	}
}

func newSnapshotProvider(client ServerClient) (*Provider, *fakeClock) {
	clock := &fakeClock{now: time.Date(2026, 11, 17, 12, 0, 0, 0, time.UTC)}
	p := NewProvider(client, "test-cluster")
	p.nowFn = clock.Now
	return p, clock
}

// The regression in #126: repeated drift checks across many nodes cost one list per
// TTL, not one GET per check.
func TestGetForDrift_RepeatedChecksAcrossNodesCostOneList(t *testing.T) {
	var servers []*hcloud.Server
	for id := int64(1); id <= 30; id++ {
		servers = append(servers, managedServer(id, 1))
	}
	client := newCountingServerClient(servers...)
	p, _ := newSnapshotProvider(client)

	for round := 0; round < 100; round++ {
		for id := int64(1); id <= 30; id++ {
			s, err := p.GetForDrift(context.Background(), FormatProviderID(id))
			if err != nil {
				t.Fatal(err)
			}
			if s == nil || s.ID != id {
				t.Fatalf("server %d: got %+v", id, s)
			}
		}
	}
	if list, get := client.counts(); list != 1 || get != 0 {
		t.Errorf("3000 drift checks on 30 nodes: want 1 list and 0 gets, got %d lists and %d gets", list, get)
	}
}

func TestGetForDrift_ConcurrentColdCacheSharesOneRefresh(t *testing.T) {
	client := newCountingServerClient(managedServer(1, 1), managedServer(2, 1))
	client.listGate = make(chan struct{})
	p, _ := newSnapshotProvider(client)

	const callers = 50
	var wg sync.WaitGroup
	errs := make(chan error, callers)
	for i := 0; i < callers; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			s, err := p.GetForDrift(context.Background(), FormatProviderID(int64(i%2+1)))
			if err == nil && s == nil {
				err = errors.New("nil server")
			}
			errs <- err
		}(i)
	}
	// Let every caller reach the in-flight refresh before it completes.
	deadline := time.Now().Add(2 * time.Second)
	for {
		if list, _ := client.counts(); list >= 1 || time.Now().After(deadline) {
			break
		}
		time.Sleep(time.Millisecond)
	}
	time.Sleep(20 * time.Millisecond)
	close(client.listGate)
	wg.Wait()
	close(errs)
	for err := range errs {
		if err != nil {
			t.Fatal(err)
		}
	}
	if list, get := client.counts(); list != 1 || get != 0 {
		t.Errorf("want 1 list and 0 gets for %d concurrent cold callers, got %d lists and %d gets", callers, list, get)
	}
}

// A real change (here the network) is seen once the snapshot expires, and not before.
func TestGetForDrift_ExpiryPicksUpChanges(t *testing.T) {
	client := newCountingServerClient(managedServer(1, 1))
	p, clock := newSnapshotProvider(client)
	ctx := context.Background()

	if s, err := p.GetForDrift(ctx, FormatProviderID(1)); err != nil || s.PrivateNet[0].Network.ID != 1 {
		t.Fatalf("first read: %+v, %v", s, err)
	}
	client.set(managedServer(1, 2))

	clock.Advance(DefaultSnapshotTTL - time.Second)
	if s, _ := p.GetForDrift(ctx, FormatProviderID(1)); s.PrivateNet[0].Network.ID != 1 {
		t.Errorf("within the TTL the snapshot must be served, got network %d", s.PrivateNet[0].Network.ID)
	}
	clock.Advance(2 * time.Second)
	if s, _ := p.GetForDrift(ctx, FormatProviderID(1)); s.PrivateNet[0].Network.ID != 2 {
		t.Errorf("after the TTL the change must be seen, got network %d", s.PrivateNet[0].Network.ID)
	}
	if list, _ := client.counts(); list != 2 {
		t.Errorf("want 2 lists (initial + one expiry), got %d", list)
	}
}

// An ID the list does not hold (deleted, created after the refresh, or without this
// cluster's labels) costs one live lookup per refresh, not one per check.
func TestGetForDrift_MissingServerLookedUpOncePerRefresh(t *testing.T) {
	unlabelled := &hcloud.Server{ID: 7}
	client := newCountingServerClient(managedServer(1, 1), unlabelled)
	p, clock := newSnapshotProvider(client)
	ctx := context.Background()

	for i := 0; i < 20; i++ {
		if s, err := p.GetForDrift(ctx, FormatProviderID(7)); err != nil || s == nil || s.ID != 7 {
			t.Fatalf("unlabelled server: %+v, %v", s, err)
		}
		if s, err := p.GetForDrift(ctx, FormatProviderID(99)); err != nil || s != nil {
			t.Fatalf("missing server must be nil, got %+v, %v", s, err)
		}
	}
	if list, get := client.counts(); list != 1 || get != 2 {
		t.Errorf("want 1 list and 2 gets (one per missing ID), got %d lists and %d gets", list, get)
	}

	clock.Advance(DefaultSnapshotTTL)
	if _, err := p.GetForDrift(ctx, FormatProviderID(99)); err != nil {
		t.Fatal(err)
	}
	if list, get := client.counts(); list != 2 || get != 3 {
		t.Errorf("a refresh must forget memoized misses: want 2 lists and 3 gets, got %d and %d", list, get)
	}
}

// A failed refresh with no previous copy is an error, never an empty list, and the
// next attempt waits out a backoff instead of retrying on every call.
func TestGetForDrift_FailedColdRefreshBacksOff(t *testing.T) {
	client := newCountingServerClient(managedServer(1, 1))
	rateLimited := hcloud.Error{Code: hcloud.ErrorCodeRateLimitExceeded, Message: "limit reached"}
	client.setListErr(rateLimited)
	p, clock := newSnapshotProvider(client)
	ctx := context.Background()

	if _, err := p.GetForDrift(ctx, FormatProviderID(1)); !hcloud.IsError(err, hcloud.ErrorCodeRateLimitExceeded) {
		t.Fatalf("want the rate-limit error, got %v", err)
	}
	for i := 0; i < 100; i++ {
		if _, err := p.GetForDrift(ctx, FormatProviderID(1)); err == nil {
			t.Fatal("a failed refresh must not read as an empty, successful snapshot")
		}
	}
	if list, get := client.counts(); list != 1 || get != 0 {
		t.Errorf("during backoff no request may be made: want 1 list and 0 gets, got %d and %d", list, get)
	}

	client.setListErr(nil)
	clock.Advance(snapshotBackoffMin)
	if s, err := p.GetForDrift(ctx, FormatProviderID(1)); err != nil || s == nil {
		t.Fatalf("after the backoff the refresh must be retried: %+v, %v", s, err)
	}
	if list, _ := client.counts(); list != 2 {
		t.Errorf("want 2 lists, got %d", list)
	}
}

// A failed refresh keeps serving the previous copy, and consecutive failures back off
// longer each time.
func TestGetForDrift_FailedRefreshKeepsPreviousCopy(t *testing.T) {
	client := newCountingServerClient(managedServer(1, 1))
	p, clock := newSnapshotProvider(client)
	ctx := context.Background()

	if _, err := p.GetForDrift(ctx, FormatProviderID(1)); err != nil {
		t.Fatal(err)
	}
	client.setListErr(errors.New("503"))
	clock.Advance(DefaultSnapshotTTL)
	for i := 0; i < 10; i++ {
		s, err := p.GetForDrift(ctx, FormatProviderID(1))
		if err != nil || s == nil || s.ID != 1 {
			t.Fatalf("stale copy must be served after a failed refresh: %+v, %v", s, err)
		}
	}
	if list, get := client.counts(); list != 2 || get != 0 {
		t.Errorf("want 2 lists and 0 gets, got %d and %d", list, get)
	}

	clock.Advance(snapshotBackoffMin) // first backoff over: retry, fail again
	_, _ = p.GetForDrift(ctx, FormatProviderID(1))
	clock.Advance(snapshotBackoffMin) // second backoff is twice as long: no request
	_, _ = p.GetForDrift(ctx, FormatProviderID(1))
	if list, _ := client.counts(); list != 3 {
		t.Errorf("second backoff must double: want 3 lists, got %d", list)
	}
	clock.Advance(snapshotBackoffMin)
	_, _ = p.GetForDrift(ctx, FormatProviderID(1))
	if list, _ := client.counts(); list != 4 {
		t.Errorf("want a retry after the doubled backoff: 4 lists, got %d", list)
	}
}

// The caller that starts a refresh may give up; the refresh still completes for the
// others.
func TestGetForDrift_CancelledFirstCallerDoesNotCancelRefresh(t *testing.T) {
	client := newCountingServerClient(managedServer(1, 1))
	client.listGate = make(chan struct{})
	p, _ := newSnapshotProvider(client)

	ctx, cancel := context.WithCancel(context.Background())
	first := make(chan error, 1)
	go func() {
		_, err := p.GetForDrift(ctx, FormatProviderID(1))
		first <- err
	}()
	for {
		if list, _ := client.counts(); list == 1 {
			break
		}
		time.Sleep(time.Millisecond)
	}
	second := make(chan error, 1)
	go func() {
		_, err := p.GetForDrift(context.Background(), FormatProviderID(1))
		second <- err
	}()
	cancel()
	if err := <-first; !errors.Is(err, context.Canceled) {
		t.Fatalf("first caller: want context.Canceled, got %v", err)
	}
	close(client.listGate)
	if err := <-second; err != nil {
		t.Fatalf("second caller must get the refresh result, got %v", err)
	}
	if list, _ := client.counts(); list != 1 {
		t.Errorf("want 1 list, got %d", list)
	}
}

func TestGetForDrift_InvalidProviderID(t *testing.T) {
	p, _ := newSnapshotProvider(newCountingServerClient())
	if _, err := p.GetForDrift(context.Background(), "aws:///i-123"); err == nil {
		t.Fatal("want a parse error")
	}
}

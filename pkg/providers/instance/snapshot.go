package instance

import (
	"context"
	"fmt"
	"sync"
	"time"

	"github.com/hetznercloud/hcloud-go/v2/hcloud"
)

// Drift checks run far more often than anything else that reads servers: Karpenter
// core re-runs drift detection for a NodeClaim on every event of any pod on its node
// (nodeclaim.disruption watches Pods). On a pool of ephemeral CI runners that is
// thousands of checks an hour, and one GET /servers/{id} each used the whole hcloud
// project budget on its own (#126). GetForDrift serves them from one shared list of
// this cluster's servers instead, so their cost no longer depends on pod churn.
const (
	// DefaultSnapshotTTL bounds how stale a drift check may be. Drift is not urgent:
	// core already requeues every NodeClaim's drift check every few minutes.
	DefaultSnapshotTTL = time.Minute
	// After a failed refresh no request is made for snapshotBackoffMin, doubling per
	// consecutive failure up to snapshotBackoffMax, so a 429 does not turn every pod
	// event into another request.
	snapshotBackoffMin = 5 * time.Second
	snapshotBackoffMax = 5 * time.Minute
)

// serverSnapshot is a cluster-scoped, TTL-bounded copy of the servers List returns.
// One refresh runs at a time and every concurrent caller waits for it. A failed refresh
// keeps the previous copy; it is never replaced by an empty one.
type serverSnapshot struct {
	list  func(ctx context.Context) ([]*hcloud.Server, error)
	get   func(ctx context.Context, id int64) (*hcloud.Server, error)
	ttl   time.Duration
	nowFn func() time.Time

	mu        sync.Mutex
	servers   map[int64]*hcloud.Server
	fetchedAt time.Time
	valid     bool
	// misses memoizes the live lookup of an ID the list did not contain (a server
	// created after the refresh, or one without this cluster's labels) until the next
	// refresh, nil meaning "not found", so such an ID costs one request per TTL.
	misses   map[int64]*hcloud.Server
	failures int
	retryAt  time.Time
	lastErr  error
	inflight *snapshotRefresh
}

type snapshotRefresh struct {
	done chan struct{}
	err  error
}

func newServerSnapshot(
	list func(ctx context.Context) ([]*hcloud.Server, error),
	get func(ctx context.Context, id int64) (*hcloud.Server, error),
	ttl time.Duration, nowFn func() time.Time,
) *serverSnapshot {
	return &serverSnapshot{list: list, get: get, ttl: ttl, nowFn: nowFn}
}

// Get returns the server with this ID, or nil when it does not exist.
func (s *serverSnapshot) Get(ctx context.Context, id int64) (*hcloud.Server, error) {
	if err := s.ensureFresh(ctx); err != nil {
		return nil, err
	}

	s.mu.Lock()
	if server, ok := s.servers[id]; ok {
		s.mu.Unlock()
		return server, nil
	}
	if server, ok := s.misses[id]; ok {
		s.mu.Unlock()
		return server, nil
	}
	generation := s.fetchedAt
	s.mu.Unlock()

	server, err := s.get(ctx, id)
	if err != nil {
		return nil, err
	}
	s.mu.Lock()
	// A refresh that finished meanwhile has newer data; do not attach this answer to it.
	if s.fetchedAt.Equal(generation) {
		if s.misses == nil {
			s.misses = map[int64]*hcloud.Server{}
		}
		s.misses[id] = server
	}
	s.mu.Unlock()
	return server, nil
}

// ensureFresh returns once the snapshot is younger than the TTL, or is stale but a
// refresh is not allowed yet (backoff), in which case the stale copy is served. With no
// copy at all during backoff it returns the last refresh error.
func (s *serverSnapshot) ensureFresh(ctx context.Context) error {
	s.mu.Lock()
	now := s.nowFn()
	if s.valid && now.Sub(s.fetchedAt) < s.ttl {
		s.mu.Unlock()
		return nil
	}
	if now.Before(s.retryAt) {
		err := s.lastErr
		valid := s.valid
		s.mu.Unlock()
		if valid {
			return nil
		}
		return fmt.Errorf("server snapshot unavailable until %s: %w", s.retryAt.Format(time.RFC3339), err)
	}
	call := s.inflight
	if call == nil {
		call = &snapshotRefresh{done: make(chan struct{})}
		s.inflight = call
		s.mu.Unlock()
		// The refresh serves every waiter, so the first caller's cancellation must not
		// end it; it keeps the caller's values and deadline budget only.
		go s.refresh(context.WithoutCancel(ctx), call)
	} else {
		s.mu.Unlock()
	}

	select {
	case <-call.done:
	case <-ctx.Done():
		return ctx.Err()
	}
	if call.err == nil {
		return nil
	}
	s.mu.Lock()
	valid := s.valid
	s.mu.Unlock()
	if valid {
		return nil
	}
	return call.err
}

func (s *serverSnapshot) refresh(ctx context.Context, call *snapshotRefresh) {
	ctx, cancel := context.WithTimeout(ctx, refreshTimeout(s.ttl))
	defer cancel()
	servers, err := s.list(ctx)

	s.mu.Lock()
	defer s.mu.Unlock()
	if err != nil {
		s.failures++
		backoff := snapshotBackoffMin << min(s.failures-1, 16)
		if backoff > snapshotBackoffMax || backoff <= 0 {
			backoff = snapshotBackoffMax
		}
		s.retryAt = s.nowFn().Add(backoff)
		s.lastErr = err
		call.err = fmt.Errorf("refreshing server snapshot: %w", err)
	} else {
		byID := make(map[int64]*hcloud.Server, len(servers))
		for _, server := range servers {
			if server != nil {
				byID[server.ID] = server
			}
		}
		s.servers = byID
		s.misses = nil
		s.fetchedAt = s.nowFn()
		s.valid = true
		s.failures = 0
		s.retryAt = time.Time{}
		s.lastErr = nil
	}
	s.inflight = nil
	close(call.done)
}

// refreshTimeout bounds one list call independently of the caller that started it.
func refreshTimeout(ttl time.Duration) time.Duration {
	if ttl < 30*time.Second {
		return 30 * time.Second
	}
	return ttl
}

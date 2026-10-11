package instance

import (
	"context"
	"errors"
	"fmt"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/hetznercloud/hcloud-go/v2/hcloud"

	apiv1 "github.com/paperclipinc/karpenter-provider-hetzner/pkg/apis/v1"
)

// spreadGroupMaxServers is how many servers Hetzner admits into one spread
// placement group. A spread guarantee only holds within a group, so a node pool
// larger than this is spread across several groups: nodes in the same group are
// on distinct hosts, nodes in different groups may share one.
const spreadGroupMaxServers = 10

// pgRefreshTTL bounds how long cached member counts are trusted. Counts only
// grow through this provider (every create is reserved against the cache) but
// shrink when servers are deleted, so an occasional re-read lets freed slots be
// reused instead of rolling over to a new group.
const pgRefreshTTL = 10 * time.Minute

// LabelPlacementGroupIndex marks a provider-managed placement group's index
// within its node pool. Index 0 is the original, un-suffixed group name.
const LabelPlacementGroupIndex = apiv1.Group + "/placement-group-index"

// ErrPlacementGroupsFull reports that every operator-supplied placement group
// is full. The provider never creates groups on the operator's behalf when
// spec.placementGroupIDs is set.
var ErrPlacementGroupsFull = errors.New("every placement group in spec.placementGroupIDs is full")

// pgEntry is one placement group of a pool, with its member count as last read
// plus any creates reserved against it since.
type pgEntry struct {
	id      int64
	index   int
	members int
}

// pgPool is the set of spread groups servers of one node pool are placed in:
// either the provider-managed, indexed groups for (cluster, node pool) or the
// explicit IDs from the NodeClass.
type pgPool struct {
	explicitIDs []int64 // set for an operator-managed pool
	base        string  // provider-managed group name for index 0
	nodePool    string
	groups      []*pgEntry
	fetched     time.Time
}

func (pool *pgPool) managed() bool { return len(pool.explicitIDs) == 0 }

// managedGroupName returns the name of the index-th provider-managed group of a
// pool. Index 0 keeps the name groups had before indexing existed, so upgrading
// keeps using the group every existing node is already in.
func managedGroupName(base string, index int) string {
	if index == 0 {
		return base
	}
	return base + "-" + strconv.Itoa(index)
}

func pgPoolKey(opts CreateOpts, base string) string {
	if len(opts.PlacementGroupIDs) == 0 {
		return "managed\x00" + base
	}
	ids := append([]int64(nil), opts.PlacementGroupIDs...)
	sort.Slice(ids, func(i, j int) bool { return ids[i] < ids[j] })
	parts := make([]string, len(ids))
	for i, id := range ids {
		parts[i] = strconv.FormatInt(id, 10)
	}
	return "explicit\x00" + strings.Join(parts, ",")
}

// acquirePlacementGroup picks the placement group a new server goes into and
// reserves a slot in it. It returns the group ID and the pool key the slot is
// reserved in; the caller must call releasePlacementGroup or
// markPlacementGroupFull if the create then fails.
//
// The least-full group with room wins, which keeps groups balanced. When no
// provider-managed group has room the counts are re-read once (deletes may have
// freed a slot) and only then is a new indexed group created. Groups in exclude
// are skipped: a create just failed against them.
func (p *Provider) acquirePlacementGroup(ctx context.Context, opts CreateOpts, exclude map[int64]bool) (int64, string, error) {
	base := placementGroupName(p.clusterName, opts.NodePool)
	key := pgPoolKey(opts, base)

	p.pgMu.Lock()
	defer p.pgMu.Unlock()
	if p.pgPools == nil {
		p.pgPools = map[string]*pgPool{}
	}
	pool, ok := p.pgPools[key]
	if !ok {
		pool = &pgPool{explicitIDs: opts.PlacementGroupIDs, base: base, nodePool: opts.NodePool}
		p.pgPools[key] = pool
	}
	refreshed := false
	if pool.fetched.IsZero() || p.now().Sub(pool.fetched) > pgRefreshTTL {
		if err := p.refreshPool(ctx, pool); err != nil {
			return 0, "", err
		}
		refreshed = true
	}

	g := pool.leastFull(exclude)
	if g == nil && !refreshed {
		if err := p.refreshPool(ctx, pool); err != nil {
			return 0, "", err
		}
		g = pool.leastFull(exclude)
	}
	if g == nil {
		if !pool.managed() {
			return 0, "", ErrPlacementGroupsFull
		}
		created, err := p.createManagedGroup(ctx, pool)
		if err != nil {
			return 0, "", err
		}
		g = created
	}
	g.members++
	return g.id, key, nil
}

// leastFull returns the group with the fewest members that still has room,
// lowest index first on a tie, or nil when none has.
func (pool *pgPool) leastFull(exclude map[int64]bool) *pgEntry {
	var best *pgEntry
	for _, g := range pool.groups {
		if exclude[g.id] || g.members >= spreadGroupMaxServers {
			continue
		}
		if best == nil || g.members < best.members || (g.members == best.members && g.index < best.index) {
			best = g
		}
	}
	return best
}

// refreshPool re-reads a pool's groups and their member counts.
func (p *Provider) refreshPool(ctx context.Context, pool *pgPool) error {
	var groups []*pgEntry
	if pool.managed() {
		found, err := p.listManagedGroups(ctx, pool)
		if err != nil {
			return err
		}
		groups = found
	} else {
		for i, id := range pool.explicitIDs {
			pg, _, err := p.pgClient.GetByID(ctx, id)
			if err != nil {
				return fmt.Errorf("getting placement group %d: %w", id, err)
			}
			if pg == nil {
				return fmt.Errorf("placement group %d from spec.placementGroupIDs does not exist", id)
			}
			groups = append(groups, &pgEntry{id: pg.ID, index: i, members: len(pg.Servers)})
		}
	}
	pool.groups = groups
	pool.fetched = p.now()
	return nil
}

// listManagedGroups finds the provider-managed groups of a pool: the original
// group by its exact name (it predates labelling, so it may carry no labels),
// plus every indexed group labelled for this cluster and node pool.
func (p *Provider) listManagedGroups(ctx context.Context, pool *pgPool) ([]*pgEntry, error) {
	byID := map[int64]*pgEntry{}

	legacy, err := p.pgClient.AllWithOpts(ctx, hcloud.PlacementGroupListOpts{
		Name: pool.base,
		Type: hcloud.PlacementGroupTypeSpread,
	})
	if err != nil {
		return nil, fmt.Errorf("listing placement groups: %w", err)
	}
	for _, pg := range legacy {
		if pg.Name == pool.base {
			byID[pg.ID] = &pgEntry{id: pg.ID, index: 0, members: len(pg.Servers)}
		}
	}

	labelled, err := p.pgClient.AllWithOpts(ctx, hcloud.PlacementGroupListOpts{
		ListOpts: hcloud.ListOpts{LabelSelector: p.groupSelector()},
		Type:     hcloud.PlacementGroupTypeSpread,
	})
	if err != nil {
		return nil, fmt.Errorf("listing placement groups: %w", err)
	}
	for _, pg := range labelled {
		// The node pool label disambiguates names: pool "a" index 2 and pool
		// "a-2" index 0 would both be called karpenter-<cluster>-a-2.
		if pg.Labels[apiv1.ServerLabelNodePool] != pool.nodePool {
			continue
		}
		idx, err := strconv.Atoi(pg.Labels[LabelPlacementGroupIndex])
		if err != nil || idx < 0 || pg.Name != managedGroupName(pool.base, idx) {
			continue
		}
		byID[pg.ID] = &pgEntry{id: pg.ID, index: idx, members: len(pg.Servers)}
	}

	out := make([]*pgEntry, 0, len(byID))
	for _, g := range byID {
		out = append(out, g)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].index < out[j].index })
	return out, nil
}

func (p *Provider) groupSelector() string {
	return fmt.Sprintf("%s=%s,%s=%s", apiv1.ServerLabelManagedBy, apiv1.ServerValueManagedBy,
		apiv1.ServerLabelCluster, p.clusterName)
}

// createManagedGroup creates the next indexed group of a pool. An index whose
// name is taken by something else (uniqueness_error) is skipped.
func (p *Provider) createManagedGroup(ctx context.Context, pool *pgPool) (*pgEntry, error) {
	next := 0
	for _, g := range pool.groups {
		if g.index >= next {
			next = g.index + 1
		}
	}
	const maxNameCollisions = 5
	for attempt := 0; attempt < maxNameCollisions; attempt, next = attempt+1, next+1 {
		labels := map[string]string{
			apiv1.ServerLabelManagedBy: apiv1.ServerValueManagedBy,
			apiv1.ServerLabelCluster:   p.clusterName,
			LabelPlacementGroupIndex:   strconv.Itoa(next),
		}
		if pool.nodePool != "" {
			labels[apiv1.ServerLabelNodePool] = pool.nodePool
		}
		name := managedGroupName(pool.base, next)
		result, _, err := p.pgClient.Create(ctx, hcloud.PlacementGroupCreateOpts{
			Name:   name,
			Type:   hcloud.PlacementGroupTypeSpread,
			Labels: labels,
		})
		if hcloud.IsError(err, hcloud.ErrorCodeUniquenessError) {
			continue
		}
		if err != nil {
			return nil, fmt.Errorf("creating placement group %q: %w", name, err)
		}
		g := &pgEntry{id: result.PlacementGroup.ID, index: next}
		pool.groups = append(pool.groups, g)
		return g, nil
	}
	return nil, fmt.Errorf("creating placement group for %q: %d consecutive names already taken", pool.base, maxNameCollisions)
}

// releasePlacementGroup returns a slot reserved by acquirePlacementGroup after
// a create that did not land in the group.
func (p *Provider) releasePlacementGroup(key string, id int64) {
	p.pgMu.Lock()
	defer p.pgMu.Unlock()
	if g := p.pgPools[key].find(id); g != nil && g.members > 0 {
		g.members--
	}
}

// markPlacementGroupFull records that hcloud refused a server for a group, so
// no further create is sent to it until the next refresh re-reads its count.
func (p *Provider) markPlacementGroupFull(key string, id int64) {
	p.pgMu.Lock()
	defer p.pgMu.Unlock()
	if g := p.pgPools[key].find(id); g != nil {
		g.members = spreadGroupMaxServers
	}
}

// forgetPlacementGroups drops a pool's cache, so the next create re-reads it.
// Used when a create suggests a cached group no longer exists.
func (p *Provider) forgetPlacementGroups(key string) {
	p.pgMu.Lock()
	defer p.pgMu.Unlock()
	delete(p.pgPools, key)
}

func (pool *pgPool) find(id int64) *pgEntry {
	if pool == nil {
		return nil
	}
	for _, g := range pool.groups {
		if g.id == id {
			return g
		}
	}
	return nil
}

func (p *Provider) now() time.Time {
	if p.nowFn != nil {
		return p.nowFn()
	}
	return time.Now()
}

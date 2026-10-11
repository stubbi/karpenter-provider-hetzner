package operator

import (
	"fmt"
	"os"
	"regexp"
	"strconv"
	"strings"
	"time"
)

var clusterNameRE = regexp.MustCompile(`^[a-zA-Z0-9._-]{1,63}$`)

const (
	// defaultAPITimeout bounds a single hcloud HTTP request. Hetzner's API
	// answers healthy requests in well under a second; 30s is generous enough
	// that a slow-but-working API is never mistaken for a hung one, and short
	// enough that a blackholed connection frees the worker within one reconcile
	// backoff rather than never.
	defaultAPITimeout = 30 * time.Second

	// maxAPITimeout caps what an operator may configure. A timeout is only useful
	// while it is shorter than the patience of everything above it; past a few
	// minutes it stops bounding anything and just reintroduces the hang it exists
	// to prevent.
	maxAPITimeout = 5 * time.Minute

	// defaultOrphanGracePeriod is how old a server must be before the orphan
	// controller may reclaim it. It has to outlast a slow boot plus core's 15m
	// registration timeout, after which core deletes an unregistered NodeClaim.
	defaultOrphanGracePeriod = 30 * time.Minute

	// minOrphanGracePeriod refuses settings short enough to reclaim a server
	// whose NodeClaim core has not yet given up on.
	minOrphanGracePeriod = 15 * time.Minute
)

// Config holds provider configuration sourced from the environment.
type Config struct {
	// ClusterName scopes all managed servers so multiple clusters can share
	// one Hetzner project without colliding.
	ClusterName string

	// APITimeout bounds a single hcloud HTTP request. It is per request, not per
	// operation: the SDK's action waiter polls in a loop of separate requests, so
	// a create that legitimately takes minutes is many short requests.
	APITimeout time.Duration

	// OrphanGCEnabled turns on reclaiming servers created for a NodeClaim that no
	// longer exists and that never became a Node.
	OrphanGCEnabled bool

	// OrphanGCGracePeriod is the minimum server age before it can be reclaimed.
	OrphanGCGracePeriod time.Duration
}

// LoadConfig reads provider configuration from the environment.
// CLUSTER_NAME is required; HCLOUD_API_TIMEOUT is optional.
func LoadConfig() (*Config, error) {
	name := strings.TrimSpace(os.Getenv("CLUSTER_NAME"))
	if name == "" {
		return nil, fmt.Errorf("CLUSTER_NAME environment variable is required")
	}
	if !clusterNameRE.MatchString(name) {
		return nil, fmt.Errorf("CLUSTER_NAME %q is not a valid Hetzner label value (must match [a-zA-Z0-9._-], max 63 chars)", name)
	}
	timeout, err := loadAPITimeout()
	if err != nil {
		return nil, err
	}
	gcEnabled, gcGrace, err := loadOrphanGC()
	if err != nil {
		return nil, err
	}
	return &Config{ClusterName: name, APITimeout: timeout, OrphanGCEnabled: gcEnabled, OrphanGCGracePeriod: gcGrace}, nil
}

// loadOrphanGC reads ORPHAN_GC_ENABLED (default true) and
// ORPHAN_GC_GRACE_PERIOD (default 30m, at least 15m). Invalid values are errors,
// not silent fallbacks, for the same reason as HCLOUD_API_TIMEOUT.
func loadOrphanGC() (bool, time.Duration, error) {
	enabled := true
	if raw := strings.TrimSpace(os.Getenv("ORPHAN_GC_ENABLED")); raw != "" {
		v, err := strconv.ParseBool(raw)
		if err != nil {
			return false, 0, fmt.Errorf("ORPHAN_GC_ENABLED %q is not a boolean: %w", raw, err)
		}
		enabled = v
	}
	grace := defaultOrphanGracePeriod
	if raw := strings.TrimSpace(os.Getenv("ORPHAN_GC_GRACE_PERIOD")); raw != "" {
		d, err := time.ParseDuration(raw)
		if err != nil {
			return false, 0, fmt.Errorf("ORPHAN_GC_GRACE_PERIOD %q is not a valid duration (e.g. \"30m\"): %w", raw, err)
		}
		if d < minOrphanGracePeriod {
			return false, 0, fmt.Errorf("ORPHAN_GC_GRACE_PERIOD %q is below the %s minimum: a shorter period can reclaim a server whose NodeClaim is still waiting to register", raw, minOrphanGracePeriod)
		}
		grace = d
	}
	return enabled, grace, nil
}

// loadAPITimeout reads HCLOUD_API_TIMEOUT as a Go duration (e.g. "45s", "1m"),
// falling back to defaultAPITimeout when unset.
//
// An unparseable or out-of-range value is an error rather than a silent fallback:
// falling back would leave the operator running with a timeout the operator who
// set it does not believe is in effect, and this is exactly the setting someone
// reaches for when the API is already misbehaving.
func loadAPITimeout() (time.Duration, error) {
	raw := strings.TrimSpace(os.Getenv("HCLOUD_API_TIMEOUT"))
	if raw == "" {
		return defaultAPITimeout, nil
	}
	d, err := time.ParseDuration(raw)
	if err != nil {
		return 0, fmt.Errorf("HCLOUD_API_TIMEOUT %q is not a valid duration (e.g. \"30s\", \"1m\"): %w", raw, err)
	}
	if d <= 0 {
		return 0, fmt.Errorf("HCLOUD_API_TIMEOUT %q must be positive; an unbounded client is what this setting exists to prevent", raw)
	}
	if d > maxAPITimeout {
		return 0, fmt.Errorf("HCLOUD_API_TIMEOUT %q exceeds the %s maximum", raw, maxAPITimeout)
	}
	return d, nil
}

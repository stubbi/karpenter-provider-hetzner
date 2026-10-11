package operator

import (
	"context"
	"fmt"
	"time"

	"github.com/hetznercloud/hcloud-go/v2/hcloud"
)

// tokenCheckTimeout bounds the startup token check: one small authenticated GET.
const tokenCheckTimeout = 30 * time.Second

// ServerLister is the narrow hcloud servers API the startup token check needs.
// *hcloud.ServerClient satisfies it.
type ServerLister interface {
	List(ctx context.Context, opts hcloud.ServerListOpts) ([]*hcloud.Server, *hcloud.Response, error)
}

// ValidateToken makes one cheap authenticated read and fails if Hetzner rejects
// the token. A wrong or revoked token otherwise only surfaces as failed creates,
// behind a green readiness probe, after the first scale-up has already stalled.
//
// Only a definitive rejection (unauthorized, forbidden) is an error. Anything
// else -- a timeout, a 5xx, a rate limit -- returns nil with the cause, so the
// caller can log it and start anyway: refusing to start during a Hetzner outage
// would turn an API blip into a crashloop. For the same reason the check runs
// once at startup and does not gate the ongoing readiness probe.
//
// A read-only token passes: hcloud reports token_readonly only on a write, and
// the check deliberately makes none. Creates with such a token fail with a
// typed HCloudTokenReadOnly reason on the NodeClaim instead.
func ValidateToken(ctx context.Context, servers ServerLister) (warning error, err error) {
	ctx, cancel := context.WithTimeout(ctx, tokenCheckTimeout)
	defer cancel()
	_, _, lerr := servers.List(ctx, hcloud.ServerListOpts{ListOpts: hcloud.ListOpts{PerPage: 1}})
	switch {
	case lerr == nil:
		return nil, nil
	case hcloud.IsError(lerr, hcloud.ErrorCodeUnauthorized):
		return nil, fmt.Errorf("HCLOUD_TOKEN was rejected by the Hetzner API (unauthorized): check that it is valid and not revoked: %w", lerr)
	case hcloud.IsError(lerr, hcloud.ErrorCodeForbidden):
		return nil, fmt.Errorf("HCLOUD_TOKEN is not allowed to list servers (forbidden): the provider needs a read/write project token: %w", lerr)
	default:
		return fmt.Errorf("could not verify HCLOUD_TOKEN at startup, continuing: %w", lerr), nil
	}
}

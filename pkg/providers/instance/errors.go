package instance

import (
	"errors"
	"fmt"
	"strings"

	"github.com/hetznercloud/hcloud-go/v2/hcloud"
	karpcp "sigs.k8s.io/karpenter/pkg/cloudprovider"
)

// ErrorClass is what a failed create means for what to do next. The class, not
// the caller, decides whether an offering is quarantined: only ClassCapacity
// names a (server type, location) pair, so a cause that is not about the
// offering can never blacklist it (#64).
type ErrorClass int

const (
	// ClassRetryable is transient (rate limits, 5xx, timeouts, an unknown
	// error): core requeues the same NodeClaim and the same offering.
	ClassRetryable ErrorClass = iota
	// ClassCapacity means this server type cannot be created in this location
	// right now. The offering is quarantined and core picks another shape.
	ClassCapacity
	// ClassQuota means the project's server quota is exhausted. No shape helps,
	// so nothing is quarantined; it surfaces on the NodeClaim and stops.
	ClassQuota
	// ClassTerminal is a condition that will not clear by retrying the same
	// request (a bad token, a misconfigured NodeClass). It surfaces on the
	// NodeClaim's Launched condition with a typed reason.
	ClassTerminal
)

// Launched-condition reasons for create errors that do not clear on retry.
const (
	ReasonProjectQuotaExceeded = "ProjectQuotaExceeded"
	ReasonUnauthorized         = "HCloudUnauthorized"
	ReasonForbidden            = "HCloudForbidden"
	ReasonTokenReadOnly        = "HCloudTokenReadOnly"
	ReasonPrivateNetOnlyServer = "PrivateNetOnlyServer"
	ReasonNoSubnetAvailable    = "NoSubnetAvailable"
)

// errorCodeTokenReadOnly is returned for a write with a read-only API token. The
// SDK has no constant for it.
const errorCodeTokenReadOnly hcloud.ErrorCode = "token_readonly"

// Classification is the outcome of ClassifyCreateError.
type Classification struct {
	Class ErrorClass
	// Reason is the Launched-condition reason for ClassQuota and ClassTerminal.
	Reason string
	// Message is a short operator-facing explanation for the condition.
	Message string
}

// ClassifyCreateError decides what a failed server create means. The default is
// retryable: an unknown cause is assumed transient, because a misclassified
// terminal error stops a launch that would have succeeded.
func ClassifyCreateError(err error) Classification {
	var he hcloud.Error
	if !errors.As(err, &he) {
		return Classification{Class: ClassRetryable}
	}
	switch he.Code {
	case hcloud.ErrorCodeResourceUnavailable:
		return Classification{Class: ClassCapacity}
	case hcloud.ErrorCodeInvalidServerType:
		// The type is still in the pricing catalogue but no longer creatable. That
		// is a fact about this server type, so quarantining the offering routes
		// around it precisely; failing the NodeClaim instead would let
		// deterministic cheapest-first selection pick the same type again.
		return Classification{Class: ClassCapacity}
	case hcloud.ErrorCodeInvalidInput:
		if isUnsupportedPlacement(he) {
			// e.g. "unsupported location for server type": priced but not
			// creatable in this location.
			return Classification{Class: ClassCapacity}
		}
	case hcloud.ErrorCodeResourceLimitExceeded:
		return Classification{Class: ClassQuota, Reason: ReasonProjectQuotaExceeded,
			Message: "Hetzner project resource limit reached (resource_limit_exceeded); raise the project limit or free servers"}
	case hcloud.ErrorCodeUnauthorized:
		return Classification{Class: ClassTerminal, Reason: ReasonUnauthorized,
			Message: "Hetzner API token is invalid or expired (unauthorized)"}
	case hcloud.ErrorCodeForbidden:
		return Classification{Class: ClassTerminal, Reason: ReasonForbidden,
			Message: "Hetzner API token is not allowed to create servers (forbidden)"}
	case errorCodeTokenReadOnly:
		return Classification{Class: ClassTerminal, Reason: ReasonTokenReadOnly,
			Message: "Hetzner API token is read-only; a read/write token is required"}
	case hcloud.ErrorCodePrivateNetOnlyServer:
		return Classification{Class: ClassTerminal, Reason: ReasonPrivateNetOnlyServer,
			Message: "a firewall cannot be attached to a server without a public interface; enable a public IP or remove firewallIDs"}
	case hcloud.ErrorCodeNoSubnetAvailable:
		// The NodeClass's network has no free subnet in this location's network
		// zone. That is about the NodeClass's network, not the server type, so
		// quarantining the offering would walk every type in the location into the
		// cache one create at a time and affect NodePools on other networks. The
		// nodeclass controller's LocationsReady check reports the missing-zone case
		// on the NodeClass too.
		return Classification{Class: ClassTerminal, Reason: ReasonNoSubnetAvailable,
			Message: "the NodeClass network has no subnet available in this location's network zone"}
	}
	// Notably retryable: rate_limit_exceeded and 5xx (transient), placement_error
	// (a full placement group is about the group, not the offering), and
	// uniqueness_error (a retry after a partial create; the right answer is to
	// adopt the server that already exists, not to fail).
	return Classification{Class: ClassRetryable}
}

// OfferingUnavailableError is a capacity error that names the offering to
// quarantine. It is an InsufficientCapacityError, so core deletes the NodeClaim
// and re-schedules onto another shape.
type OfferingUnavailableError struct {
	*karpcp.InsufficientCapacityError
	ServerType string
	Location   string
}

// Unwrap exposes both the core capacity error and the hcloud cause.
func (e *OfferingUnavailableError) Unwrap() error { return e.InsufficientCapacityError }

// QuarantineTarget returns the offering a create error says is unavailable, if
// any. It is the only source of offerings to mark unavailable after a failed
// create: a generic IsInsufficientCapacityError check would also match causes
// that are not about the offering.
func QuarantineTarget(err error) (serverType, location string, ok bool) {
	var oe *OfferingUnavailableError
	if !errors.As(err, &oe) {
		return "", "", false
	}
	return oe.ServerType, oe.Location, true
}

// MapCreateError converts a Hetzner create error for the given offering into the
// error Karpenter core acts on. nil passes through as nil.
//
//   - capacity: an OfferingUnavailableError (an InsufficientCapacityError naming
//     the offering), so core routes around it and the provider quarantines it;
//   - quota and terminal: a CreateError carrying a typed reason, so the cause is
//     visible on the NodeClaim's Launched condition instead of being retried as
//     if it were capacity;
//   - anything else: a plain error, which core requeues.
func MapCreateError(err error, serverType, location string) error {
	if err == nil {
		return nil
	}
	c := ClassifyCreateError(err)
	switch c.Class {
	case ClassCapacity:
		return &OfferingUnavailableError{
			InsufficientCapacityError: karpcp.NewInsufficientCapacityError(err),
			ServerType:                serverType,
			Location:                  location,
		}
	case ClassQuota, ClassTerminal:
		return karpcp.NewCreateError(fmt.Errorf("creating server: %w", err), c.Reason, c.Message)
	default:
		return fmt.Errorf("creating server: %w", err)
	}
}

// isUnsupportedPlacement reports whether the error is Hetzner rejecting a
// (server type, location) combination as not creatable. Hetzner returns this as
// a generic invalid_input, so we match the placement-specific message rather
// than treating every invalid_input as a capacity signal.
func isUnsupportedPlacement(he hcloud.Error) bool {
	if he.Code != hcloud.ErrorCodeInvalidInput {
		return false
	}
	m := strings.ToLower(he.Message)
	return strings.Contains(m, "unsupported location") ||
		strings.Contains(m, "location for server type")
}

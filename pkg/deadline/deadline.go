// Package deadline holds the context budgets the provider applies to each class
// of hcloud work.
//
// controller-runtime imposes no per-reconcile deadline, so without these every
// call inherits an open-ended context. The HTTP client (#77) already bounds each
// single request; these bound an OPERATION, which is many requests: the SDK
// retries a failed request up to five times with backoff, and the action waiter
// polls in a loop of separate requests.
//
// The budgets are deliberately not uniform. A server create legitimately blocks
// for the whole boot, which is minutes on a slow day, while a read is a handful
// of GETs. One deadline short enough to bound the reads would cancel healthy
// launches; one long enough for launches would bound nothing else.
package deadline

import "time"

const (
	// Read bounds a read-only operation: Get, List, IsDrifted, GetInstanceTypes
	// (including a catalogue refresh) and one nodeclass status reconcile. Each is
	// at most a few requests; two minutes covers a request timing out at the 30s
	// HTTP default and being retried with the SDK's backoff, and frees the worker
	// well before the next five-minute resync.
	Read = 2 * time.Minute

	// Delete bounds a server delete: one GET and one DELETE. Deletion is
	// asynchronous at Hetzner, so nothing here waits for the server to go away,
	// and a failed attempt is simply retried by core's termination controller.
	Delete = 1 * time.Minute

	// CreateSubmit bounds everything in a create up to and including hcloud
	// accepting the request: resolving the node class, instance types, image and
	// userData, resolving the placement group, and the servers.Create call. None
	// of that waits on the server itself.
	CreateSubmit = 2 * time.Minute

	// BootWait bounds waiting for the create action (and its follow-up actions)
	// once hcloud has accepted the create. It is a separate budget derived from
	// the caller's context, not from the submission context, so a slow
	// submission cannot eat into the boot and the submission deadline can never
	// cancel a server that already exists. Hetzner create actions complete in
	// well under two minutes in practice; ten minutes leaves wide headroom while
	// staying below core's 15-minute registration TTL, after which the NodeClaim
	// is deleted anyway.
	BootWait = 10 * time.Minute
)

// Budgets carries the per-class budgets. Production uses Defaults; tests shrink
// them to observe a hung call returning on its budget.
type Budgets struct {
	Read         time.Duration
	Delete       time.Duration
	CreateSubmit time.Duration
	BootWait     time.Duration
}

// Defaults returns the production budgets.
func Defaults() Budgets {
	return Budgets{Read: Read, Delete: Delete, CreateSubmit: CreateSubmit, BootWait: BootWait}
}

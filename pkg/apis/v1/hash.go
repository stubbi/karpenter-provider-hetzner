package v1

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"slices"
)

const (
	// AnnotationNodeClassHash records the hash of the HCloudNodeClass fields a
	// server cannot report back once it is running. It is stamped on the
	// HCloudNodeClass (for visibility) and on every NodeClaim it launches; drift
	// compares the NodeClaim's value against the NodeClass's current spec.
	AnnotationNodeClassHash = Group + "/nodeclass-hash"

	// AnnotationNodeClassHashVersion records which hashing scheme produced
	// AnnotationNodeClassHash. Drift only compares hashes of the same version, so
	// changing what is hashed (bumping NodeClassHashVersion) can never roll a
	// fleet by itself: NodeClaims on the old version are re-stamped instead.
	AnnotationNodeClassHashVersion = Group + "/nodeclass-hash-version"

	// NodeClassHashVersion is the current hashing scheme. Bump it whenever the
	// set of hashed fields or their encoding changes.
	NodeClassHashVersion = "v1"
)

// nodeClassHashInput is the part of the spec that changes how a server is built
// but cannot be read back from the server afterwards, so the live drift checks
// cannot see it. Everything observable on the server (network, firewalls,
// server type, location, labels) stays with the live checks instead, which also
// catch out-of-band changes a hash cannot.
//
// Deliberately absent:
//   - locations: a node already in an allowed location is fine; the live
//     location check rolls only nodes that are no longer allowed.
//   - imageSelector: image drift compares resolved image IDs and only when the
//     selector pins (see IsDrifted), so an unpinned selector never rolls a fleet
//     on someone else's snapshot upload.
//   - the userData Secret's contents: only the reference is hashed. Rotating a
//     Secret's value is not a deliberate change to the node class, and hashing
//     it would roll every node whenever a token is rotated. Changing the
//     reference is the explicit gesture.
//   - kubelet and vmMemoryOverheadPercent, unless spec.bootstrap is set: both
//     describe the node to the scheduler without changing what is booted, so a
//     change re-models capacity for new nodes rather than requiring the old ones
//     to be replaced. With spec.bootstrap the provider renders kubelet into the
//     node's flags, so it is hashed together with the bootstrap block.
//   - the join token Secret's contents: as with userData, only the reference is
//     hashed, so rotating a token never rolls the fleet.
//
// Bootstrap and Kubelet are omitted from the encoding when unset, so node
// classes without spec.bootstrap hash exactly as before it existed.
//
// Field order and JSON encoding are part of the hash: changing either needs a
// NodeClassHashVersion bump.
//
// +kubebuilder:object:generate=false
type nodeClassHashInput struct {
	UserData               string                   `json:"userData,omitempty"`
	UserDataSecretRef      *UserDataSecretReference `json:"userDataSecretRef,omitempty"`
	SSHKeyIDs              []int64                  `json:"sshKeyIDs,omitempty"`
	PlacementGroupStrategy string                   `json:"placementGroupStrategy"`
	EnablePublicIPv4       bool                     `json:"enablePublicIPv4"`
	EnablePublicIPv6       bool                     `json:"enablePublicIPv6"`
	Bootstrap              *BootstrapConfiguration  `json:"bootstrap,omitempty"`
	Kubelet                *KubeletConfiguration    `json:"kubelet,omitempty"`
}

// Hash returns the hash of the node class fields that drift cannot observe on a
// running server. Equivalent specs hash equally: defaults are applied (an unset
// placement strategy is spread, unset public IPs are enabled) and SSH key order
// is ignored, so an edit that changes nothing a server would see rolls nothing.
func (in *HCloudNodeClass) Hash() string {
	keys := slices.Clone(in.Spec.SSHKeyIDs)
	slices.Sort(keys)
	pg := in.Spec.PlacementGroupStrategy
	if pg == "" {
		pg = "spread"
	}
	var kubelet *KubeletConfiguration
	if in.Spec.Bootstrap != nil {
		kubelet = in.Spec.Kubelet
	}
	b, err := json.Marshal(nodeClassHashInput{
		UserData:               in.Spec.UserData,
		UserDataSecretRef:      in.Spec.UserDataSecretRef,
		SSHKeyIDs:              keys,
		PlacementGroupStrategy: pg,
		EnablePublicIPv4:       in.Spec.PublicIPv4Enabled(),
		EnablePublicIPv6:       in.Spec.PublicIPv6Enabled(),
		Bootstrap:              in.Spec.Bootstrap,
		Kubelet:                kubelet,
	})
	if err != nil {
		// Unreachable: every field is a plain string, int, bool or struct of them.
		panic(err)
	}
	sum := sha256.Sum256(b)
	return hex.EncodeToString(sum[:8])
}

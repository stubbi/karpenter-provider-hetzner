package v1

import (
	"github.com/awslabs/operatorpkg/status"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

// Status condition types for HCloudNodeClass.
const (
	ConditionTypeImagesReady    = "ImagesReady"
	ConditionTypeLocationsReady = "LocationsReady"
	ConditionTypeNetworkReady   = "NetworkReady"
	ConditionTypeResourcesReady = "ResourcesReady"
	ConditionTypeUserDataReady  = "UserDataReady"
)

// +kubebuilder:object:root=true
// +kubebuilder:subresource:status
// +kubebuilder:resource:categories=karpenter,shortName=hcnc,scope=Cluster
// +kubebuilder:printcolumn:name="Ready",type="string",JSONPath=".status.conditions[?(@.type=='Ready')].status"
type HCloudNodeClass struct {
	metav1.TypeMeta   `json:",inline"`
	metav1.ObjectMeta `json:"metadata,omitempty"`
	Spec              HCloudNodeClassSpec   `json:"spec,omitempty"`
	Status            HCloudNodeClassStatus `json:"status,omitempty"`
}

type HCloudNodeClassSpec struct {
	// +kubebuilder:validation:MinItems=1
	Locations []string `json:"locations"`

	ImageSelector ImageSelector `json:"imageSelector"`

	// +kubebuilder:validation:Minimum=1
	NetworkID int64 `json:"networkID"`

	// +optional
	FirewallIDs []int64 `json:"firewallIDs,omitempty"`

	// +optional
	SSHKeyIDs []int64 `json:"sshKeyIDs,omitempty"`

	// +kubebuilder:default=spread
	// +kubebuilder:validation:Enum=spread;none
	// +optional
	PlacementGroupStrategy string `json:"placementGroupStrategy,omitempty"`

	// +optional
	Labels map[string]string `json:"labels,omitempty"`

	// +optional
	UserData string `json:"userData,omitempty"`

	// UserDataSecretRef sources userData from a Secret instead of inline. When
	// set, it takes precedence over UserData. The Secret is read at server-create
	// time; its value never appears in the NodeClass spec, status, or git.
	// +optional
	UserDataSecretRef *UserDataSecretReference `json:"userDataSecretRef,omitempty"`

	// EnablePublicIPv4 controls whether created servers get a public IPv4.
	// Defaults to true (Hetzner's default). Set false on private-network
	// clusters to avoid the primary-IPv4 charge.
	// +kubebuilder:default=true
	// +optional
	EnablePublicIPv4 *bool `json:"enablePublicIPv4,omitempty"`

	// EnablePublicIPv6 controls whether created servers get a public IPv6.
	// Defaults to true. Set false to drop the public IPv6 as well.
	// +kubebuilder:default=true
	// +optional
	EnablePublicIPv6 *bool `json:"enablePublicIPv6,omitempty"`

	// Kubelet declares the reservations this node class's bootstrap applies to
	// the kubelet, so Karpenter can subtract them when computing how much of a
	// server a pod can actually use.
	//
	// This provider does not render the bootstrap: userData is supplied by the
	// operator, so these values describe what that userData does rather than
	// configure it. They must be kept in agreement with it. Declaring less than
	// the bootstrap reserves makes Karpenter believe a server is larger than it
	// is, which strands pods on nodes too small for them.
	//
	// When omitted, a flat 100m CPU / 100Mi memory kubeReserved is assumed.
	// +optional
	Kubelet *KubeletConfiguration `json:"kubelet,omitempty"`
}

// KubeletConfiguration mirrors the subset of kubelet settings that change how
// much of a node is schedulable. Only the reservation knobs are modelled;
// anything that does not move allocatable is deliberately absent.
type KubeletConfiguration struct {
	// SystemReserved is resources reserved for OS system daemons, matching the
	// kubelet's --system-reserved.
	// +kubebuilder:validation:XValidation:message="valid keys for systemReserved are ['cpu','memory','ephemeral-storage','pid']",rule="self.all(x, x=='cpu' || x=='memory' || x=='ephemeral-storage' || x=='pid')"
	// +optional
	SystemReserved map[string]ReservedQuantity `json:"systemReserved,omitempty"`

	// KubeReserved is resources reserved for Kubernetes system daemons, matching
	// the kubelet's --kube-reserved.
	// +kubebuilder:validation:XValidation:message="valid keys for kubeReserved are ['cpu','memory','ephemeral-storage','pid']",rule="self.all(x, x=='cpu' || x=='memory' || x=='ephemeral-storage' || x=='pid')"
	// +optional
	KubeReserved map[string]ReservedQuantity `json:"kubeReserved,omitempty"`

	// EvictionHard is the kubelet's hard eviction thresholds. Memory held back
	// for eviction is memory a pod cannot have, so it comes out of allocatable
	// the same way a reservation does. Values are either a quantity ("400Mi") or
	// a percentage of capacity ("10%"). A signal left out is assumed at the
	// kubelet default (memory.available=100Mi, nodefs.available=10%), which errs
	// toward underselling the node; declare "0%" (the kubelet's own spelling) for
	// a signal the kubelet does not enforce.
	//
	// Soft eviction is deliberately not modelled: it is a warning threshold that
	// does not reduce allocatable.
	// +kubebuilder:validation:XValidation:message="valid keys for evictionHard are ['memory.available','allocatableMemory.available','nodefs.available','nodefs.inodesFree','imagefs.available','imagefs.inodesFree','containerfs.available','containerfs.inodesFree','pid.available']",rule="self.all(x, x in ['memory.available','allocatableMemory.available','nodefs.available','nodefs.inodesFree','imagefs.available','imagefs.inodesFree','containerfs.available','containerfs.inodesFree','pid.available'])"
	// +optional
	EvictionHard map[string]EvictionThreshold `json:"evictionHard,omitempty"`
}

// ReservedQuantity is a non-negative resource quantity, such as "512Mi" or "200m".
// +kubebuilder:validation:Pattern=`^[+]?(([0-9]+([.][0-9]*)?)|([.][0-9]+))(([KMGTPE]i)|[numkMGTPE]|([eE]([+]|-)?[0-9]{1,3}))?$`
// +kubebuilder:validation:MaxLength=32
type ReservedQuantity string

// EvictionThreshold is a hard eviction threshold: a non-negative quantity such as
// "400Mi", or a percentage of capacity such as "10%". The kubelet flag syntax
// ("memory.available<400Mi") is not accepted; give the value alone.
// +kubebuilder:validation:Pattern=`^(([0-9]{1,2}([.][0-9]*)?|[.][0-9]+|100([.]0*)?)%|[+]?(([0-9]+([.][0-9]*)?)|([.][0-9]+))(([KMGTPE]i)|[numkMGTPE]|([eE]([+]|-)?[0-9]{1,3}))?)$`
// +kubebuilder:validation:MaxLength=32
type EvictionThreshold string

// UserDataSecretReference points at a Secret key holding the server userData.
type UserDataSecretReference struct {
	// Namespace of the Secret (required: HCloudNodeClass is cluster-scoped).
	Namespace string `json:"namespace"`
	// Name of the Secret.
	Name string `json:"name"`
	// Key within the Secret's data holding the userData.
	Key string `json:"key"`
}

type ImageSelector struct {
	// +kubebuilder:validation:Enum=talos;ubuntu
	Family string `json:"family"`

	// +optional
	Version string `json:"version,omitempty"`

	// Selector is an hcloud label selector applied when listing images, e.g.
	// {"caph-image-name": "talos-v1.13.3-gvisor"}. Use it to pin the exact image
	// (version plus baked extensions) instead of fuzzy description matching.
	// +optional
	Selector map[string]string `json:"selector,omitempty"`
}

type ResolvedImage struct {
	// Architecture is the hcloud architecture spelling ("x86" or "arm"), not the
	// Kubernetes one ("amd64"/"arm64"). Instance-type selection compares this value
	// exactly, so an unrecognised spelling would make every architecture ineligible
	// while all conditions stayed green. Deliberately not a CRD enum: writer and reader
	// share the SDK constants so the drift cannot occur, while an enum would reject every
	// status update wholesale on clusters whose CRDs were applied at install and never
	// upgraded, the first time a third architecture appears.
	Architecture string `json:"architecture"`
	ImageID      int64  `json:"imageID"`
}

type HCloudNodeClassStatus struct {
	// +optional
	Conditions []status.Condition `json:"conditions,omitempty"`
	// ResolvedImages holds one image ID per hcloud architecture, resolved from
	// Spec.ImageSelector. The generation these answer is not stored alongside them: it is
	// read back from the ImagesReady condition's observedGeneration, which operatorpkg
	// stamps on every condition write and which has been part of the shipped CRD since
	// v1. A dedicated status field would be pruned to zero by any apiserver still serving
	// the CRD from an earlier release -- Helm never upgrades crds/ -- which would make
	// the carry-forward below silently inert on exactly the clusters it protects.
	// +optional
	ResolvedImages []ResolvedImage `json:"resolvedImages,omitempty"`
}

var conditionTypes = status.NewReadyConditions(ConditionTypeImagesReady, ConditionTypeLocationsReady, ConditionTypeNetworkReady, ConditionTypeResourcesReady, ConditionTypeUserDataReady)

func (in *HCloudNodeClass) GetConditions() []status.Condition {
	return in.Status.Conditions
}

func (in *HCloudNodeClass) SetConditions(conditions []status.Condition) {
	in.Status.Conditions = conditions
}

func (in *HCloudNodeClass) StatusConditions(opts ...status.ForOption) status.ConditionSet {
	return conditionTypes.For(in, opts...)
}

// PublicIPv4Enabled reports whether public IPv4 should be enabled (default true).
func (s HCloudNodeClassSpec) PublicIPv4Enabled() bool {
	return s.EnablePublicIPv4 == nil || *s.EnablePublicIPv4
}

// PublicIPv6Enabled reports whether public IPv6 should be enabled (default true).
func (s HCloudNodeClassSpec) PublicIPv6Enabled() bool {
	return s.EnablePublicIPv6 == nil || *s.EnablePublicIPv6
}

// +kubebuilder:object:root=true
type HCloudNodeClassList struct {
	metav1.TypeMeta `json:",inline"`
	metav1.ListMeta `json:"metadata,omitempty"`
	Items           []HCloudNodeClass `json:"items"`
}

// ResolvedImagesGeneration returns the metadata.generation that the entries currently
// in Status.ResolvedImages were resolved under, read from the ImagesReady condition's
// observedGeneration. operatorpkg stamps that on every condition write, and the
// nodeclass controller always writes the condition in the same pass it writes
// ResolvedImages, so the two stay in step.
//
// Reads the raw condition slice rather than StatusConditions().Get: the ConditionSet
// constructor initializes any absent dependent condition to Unknown stamped with the
// CURRENT generation, so going through it would both mutate the object and report a
// never-resolved NodeClass as up to date. Absent therefore reads as zero, which fails
// safe -- a generation of zero never matches a real one, so callers discard rather
// than trust the entries.
func (in *HCloudNodeClass) ResolvedImagesGeneration() int64 {
	for _, cond := range in.Status.Conditions {
		if cond.Type == ConditionTypeImagesReady {
			return cond.ObservedGeneration
		}
	}
	return 0
}

// CurrentResolvedImages returns Status.ResolvedImages when they were resolved under the
// current spec generation, and nil otherwise. Consumers must not launch an image that
// answers a superseded Spec.ImageSelector: nothing upstream enforces this, because
// karpenter core's NodeClass readiness gate compares no observedGeneration, so a
// NodeClass whose spec was just edited still reads Ready=True until the nodeclass
// controller reconciles it -- and stays that way indefinitely if that controller is
// wedged. Discarding stale entries falls back to a live lookup against the current
// selector, which is what this provider did before status was consulted at all.
func (in *HCloudNodeClass) CurrentResolvedImages() []ResolvedImage {
	if in.ResolvedImagesGeneration() != in.Generation {
		return nil
	}
	return in.Status.ResolvedImages
}

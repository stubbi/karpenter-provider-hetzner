package instance

import (
	"errors"
	"fmt"
	"testing"

	"github.com/hetznercloud/hcloud-go/v2/hcloud"
	karpcp "sigs.k8s.io/karpenter/pkg/cloudprovider"
)

func TestMapCreateError_Taxonomy(t *testing.T) {
	type want struct {
		capacity bool   // InsufficientCapacityError naming the offering
		reason   string // CreateError Launched reason; "" means not a CreateError
	}
	cases := map[string]struct {
		in   error
		want want
	}{
		"resource_unavailable": {hcloud.Error{Code: hcloud.ErrorCodeResourceUnavailable}, want{capacity: true}},
		"invalid_server_type":  {hcloud.Error{Code: hcloud.ErrorCodeInvalidServerType}, want{capacity: true}},
		"unsupported location": {hcloud.Error{Code: hcloud.ErrorCodeInvalidInput, Message: "unsupported location for server type"}, want{capacity: true}},
		"quota":                {hcloud.Error{Code: hcloud.ErrorCodeResourceLimitExceeded}, want{reason: ReasonProjectQuotaExceeded}},
		"unauthorized":         {hcloud.Error{Code: hcloud.ErrorCodeUnauthorized}, want{reason: ReasonUnauthorized}},
		"forbidden":            {hcloud.Error{Code: hcloud.ErrorCodeForbidden}, want{reason: ReasonForbidden}},
		"token_readonly":       {hcloud.Error{Code: "token_readonly"}, want{reason: ReasonTokenReadOnly}},
		"private_net_only":     {hcloud.Error{Code: hcloud.ErrorCodePrivateNetOnlyServer}, want{reason: ReasonPrivateNetOnlyServer}},
		"no_subnet_available":  {hcloud.Error{Code: hcloud.ErrorCodeNoSubnetAvailable}, want{reason: ReasonNoSubnetAvailable}},
		"rate limit":           {hcloud.Error{Code: hcloud.ErrorCodeRateLimitExceeded}, want{}},
		"placement_error":      {hcloud.Error{Code: hcloud.ErrorCodePlacementError}, want{}},
		"uniqueness_error":     {hcloud.Error{Code: hcloud.ErrorCodeUniquenessError}, want{}},
		"other invalid_input":  {hcloud.Error{Code: hcloud.ErrorCodeInvalidInput, Message: "server name is invalid"}, want{}},
		"non-hcloud":           {errors.New("boom"), want{}},
		"wrapped quota":        {fmt.Errorf("ctx: %w", hcloud.Error{Code: hcloud.ErrorCodeResourceLimitExceeded}), want{reason: ReasonProjectQuotaExceeded}},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			got := MapCreateError(tc.in, "cx22", "nbg1")
			if got == nil {
				t.Fatal("non-nil error mapped to nil")
			}
			if c := karpcp.IsInsufficientCapacityError(got); c != tc.want.capacity {
				t.Errorf("IsInsufficientCapacityError = %v, want %v (%v)", c, tc.want.capacity, got)
			}
			st, loc, ok := QuarantineTarget(fmt.Errorf("creating server: %w", got))
			if ok != tc.want.capacity {
				t.Errorf("QuarantineTarget ok = %v, want %v", ok, tc.want.capacity)
			}
			if ok && (st != "cx22" || loc != "nbg1") {
				t.Errorf("QuarantineTarget = (%q, %q), want the offering that failed", st, loc)
			}
			var ce *karpcp.CreateError
			isCreate := errors.As(got, &ce)
			if (tc.want.reason != "") != isCreate {
				t.Fatalf("CreateError = %v, want reason %q (%v)", isCreate, tc.want.reason, got)
			}
			if isCreate && (ce.ConditionReason != tc.want.reason || ce.ConditionMessage == "") {
				t.Errorf("CreateError reason/message = %q/%q, want %q and a message", ce.ConditionReason, ce.ConditionMessage, tc.want.reason)
			}
			// The hcloud cause stays in the chain for errorCode and the metrics.
			var he hcloud.Error
			if errors.As(tc.in, &he) && !errors.As(got, &he) {
				t.Error("hcloud cause lost from the error chain")
			}
		})
	}
	if MapCreateError(nil, "cx22", "nbg1") != nil {
		t.Error("nil should map to nil")
	}
}

// Quota is a project-wide condition: it must never be an offering signal.
func TestQuotaIsNeverCapacity(t *testing.T) {
	err := MapCreateError(hcloud.Error{Code: hcloud.ErrorCodeResourceLimitExceeded}, "cx22", "nbg1")
	if karpcp.IsInsufficientCapacityError(err) {
		t.Fatal("quota mapped to InsufficientCapacityError; core would route around it offering by offering")
	}
	if _, _, ok := QuarantineTarget(err); ok {
		t.Fatal("quota names an offering to quarantine")
	}
}

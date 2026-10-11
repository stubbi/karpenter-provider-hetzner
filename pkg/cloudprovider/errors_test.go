package cloudprovider_test

import (
	"context"
	"errors"
	"testing"

	"github.com/hetznercloud/hcloud-go/v2/hcloud"
	karpcp "sigs.k8s.io/karpenter/pkg/cloudprovider"

	apiv1 "github.com/paperclipinc/karpenter-provider-hetzner/pkg/apis/v1"
	"github.com/paperclipinc/karpenter-provider-hetzner/pkg/providers/instance"
)

func cx32Type() *hcloud.ServerType {
	t := cx22Type()
	t.Name, t.Cores, t.Memory, t.Disk = "cx32", 4, 8, 80
	t.Pricings = []hcloud.ServerTypeLocationPricing{{
		Location: &hcloud.Location{Name: "nbg1"},
		Hourly:   hcloud.Price{Net: "0.0120"},
		Monthly:  hcloud.Price{Net: "7.5000"},
	}}
	return t
}

// The amplification in #64: quota exhaustion was mapped to capacity, so each
// failed create blacklisted the cheapest offering and the next attempt walked to
// the next one, until the whole catalogue was quarantined. A quota failure must
// leave every offering available, so the next attempt chooses the same type.
func TestCreate_QuotaFailureDoesNotQuarantineOfferings(t *testing.T) {
	cp, fsc, typeProvider := buildCPWithTypes(t, baselineNodeClass(), []*hcloud.ServerType{cx22Type(), cx32Type()})
	fsc.createErr = hcloud.Error{Code: hcloud.ErrorCodeResourceLimitExceeded, Message: "server limit reached"}

	_, err := cp.Create(context.Background(), createNodeClaim())
	if err == nil {
		t.Fatal("expected the quota failure to surface")
	}
	if karpcp.IsInsufficientCapacityError(err) {
		t.Fatal("quota surfaced as InsufficientCapacityError; core would delete the NodeClaim and walk the catalogue")
	}
	var ce *karpcp.CreateError
	if !errors.As(err, &ce) || ce.ConditionReason != instance.ReasonProjectQuotaExceeded {
		t.Fatalf("err = %v, want a CreateError with reason %s for the Launched condition", err, instance.ReasonProjectQuotaExceeded)
	}

	its, lerr := typeProvider.List(context.Background(), &apiv1.HCloudNodeClass{Spec: apiv1.HCloudNodeClassSpec{Locations: []string{"nbg1"}}})
	if lerr != nil {
		t.Fatal(lerr)
	}
	for _, it := range its {
		for _, o := range it.Offerings {
			if !o.Available {
				t.Errorf("%s/%s was quarantined by a project quota error", it.Name, o.Zone())
			}
		}
	}

	// The second attempt, once quota is freed, picks the same (cheapest) type
	// rather than the next one down a blacklist.
	fsc.createErr = nil
	out, err := cp.Create(context.Background(), createNodeClaim())
	if err != nil {
		t.Fatalf("second create: %v", err)
	}
	if got := fsc.lastOpts.ServerType.Name; got != "cx22" {
		t.Errorf("second attempt launched %s, want cx22: the first offering was still blacklisted", got)
	}
	if out.Status.ProviderID == "" {
		t.Error("second attempt returned no provider ID")
	}
}

// Terminal causes are not about the offering either.
func TestCreate_TerminalErrorsSurfaceWithReasonAndQuarantineNothing(t *testing.T) {
	for code, reason := range map[hcloud.ErrorCode]string{
		hcloud.ErrorCodeUnauthorized:         instance.ReasonUnauthorized,
		hcloud.ErrorCodeForbidden:            instance.ReasonForbidden,
		"token_readonly":                     instance.ReasonTokenReadOnly,
		hcloud.ErrorCodePrivateNetOnlyServer: instance.ReasonPrivateNetOnlyServer,
		hcloud.ErrorCodeNoSubnetAvailable:    instance.ReasonNoSubnetAvailable,
	} {
		t.Run(string(code), func(t *testing.T) {
			cp, fsc, typeProvider := buildCPWithTypes(t, baselineNodeClass(), []*hcloud.ServerType{cx22Type()})
			fsc.createErr = hcloud.Error{Code: code}
			_, err := cp.Create(context.Background(), createNodeClaim())
			var ce *karpcp.CreateError
			if !errors.As(err, &ce) || ce.ConditionReason != reason {
				t.Fatalf("err = %v, want CreateError reason %s", err, reason)
			}
			its, _ := typeProvider.List(context.Background(), &apiv1.HCloudNodeClass{Spec: apiv1.HCloudNodeClassSpec{Locations: []string{"nbg1"}}})
			for _, it := range its {
				for _, o := range it.Offerings {
					if !o.Available {
						t.Errorf("%s/%s quarantined by %s", it.Name, o.Zone(), code)
					}
				}
			}
		})
	}
}

// A capacity error still quarantines exactly the offering it names, so the next
// attempt routes to the next shape.
func TestCreate_CapacityErrorQuarantinesOnlyThatOffering(t *testing.T) {
	cp, fsc, typeProvider := buildCPWithTypes(t, baselineNodeClass(), []*hcloud.ServerType{cx22Type(), cx32Type()})
	fsc.createErr = hcloud.Error{Code: hcloud.ErrorCodeResourceUnavailable}
	if _, err := cp.Create(context.Background(), createNodeClaim()); !karpcp.IsInsufficientCapacityError(err) {
		t.Fatalf("err = %v, want InsufficientCapacityError", err)
	}
	its, _ := typeProvider.List(context.Background(), &apiv1.HCloudNodeClass{Spec: apiv1.HCloudNodeClassSpec{Locations: []string{"nbg1"}}})
	for _, it := range its {
		for _, o := range it.Offerings {
			if want := it.Name != "cx22"; o.Available != want {
				t.Errorf("%s/%s available = %v, want %v", it.Name, o.Zone(), o.Available, want)
			}
		}
	}
	fsc.createErr = nil
	if _, err := cp.Create(context.Background(), createNodeClaim()); err != nil {
		t.Fatal(err)
	}
	if got := fsc.lastOpts.ServerType.Name; got != "cx32" {
		t.Errorf("after quarantining cx22 the next attempt launched %s, want cx32", got)
	}
}

package v1_test

import (
	"context"
	"fmt"
	"os"
	"testing"

	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/envtest"
)

// TestCRD_KubeletAdmission installs the shipped CRD on a real API server and
// checks spec.kubelet admission end to end: the schema must compile and fit the
// CEL cost budget, and the key rules must admit every kubelet signal while
// refusing anything else. Needs envtest binaries; run via make test-envtest.
func TestCRD_KubeletAdmission(t *testing.T) {
	if os.Getenv("KUBEBUILDER_ASSETS") == "" {
		t.Skip("KUBEBUILDER_ASSETS not set; run make test-envtest")
	}
	env := &envtest.Environment{
		CRDDirectoryPaths:     []string{"../../../charts/karpenter-provider-hetzner/crds"},
		ErrorIfCRDPathMissing: true,
	}
	cfg, err := env.Start()
	if err != nil {
		t.Fatalf("installing the CRDs: %v", err)
	}
	t.Cleanup(func() { _ = env.Stop() })
	c, err := client.New(cfg, client.Options{})
	if err != nil {
		t.Fatal(err)
	}

	n := 0
	create := func(kubelet map[string]any) error {
		n++
		return c.Create(context.Background(), &unstructured.Unstructured{Object: map[string]any{
			"apiVersion": "karpenter.hetzner.cloud/v1",
			"kind":       "HCloudNodeClass",
			"metadata":   map[string]any{"name": fmt.Sprintf("nc-%d", n)},
			"spec": map[string]any{
				"locations":     []any{"nbg1"},
				"networkID":     int64(1),
				"imageSelector": map[string]any{"family": "ubuntu"},
				"kubelet":       kubelet,
			},
		}})
	}

	allSignals := map[string]any{}
	for _, s := range []string{"memory.available", "allocatableMemory.available", "nodefs.available", "nodefs.inodesFree",
		"imagefs.available", "imagefs.inodesFree", "containerfs.available", "containerfs.inodesFree", "pid.available"} {
		allSignals[s] = "10%"
	}
	for name, tc := range map[string]struct {
		kubelet map[string]any
		admit   bool
	}{
		"every kubelet eviction signal": {map[string]any{"evictionHard": allSignals}, true},
		"every reservation key": {map[string]any{
			"systemReserved": map[string]any{"cpu": "200m", "memory": "512Mi", "ephemeral-storage": "1Gi", "pid": "100"},
			"kubeReserved":   map[string]any{"cpu": "200m", "memory": "512Mi", "ephemeral-storage": "1Gi", "pid": "100"},
		}, true},
		"unknown eviction signal":  {map[string]any{"evictionHard": map[string]any{"memory.free": "1Gi"}}, false},
		"unknown reservation key":  {map[string]any{"kubeReserved": map[string]any{"gpu": "1"}}, false},
		"kubelet flag syntax":      {map[string]any{"evictionHard": map[string]any{"memory.available": "<400Mi"}}, false},
		"non-quantity reservation": {map[string]any{"systemReserved": map[string]any{"memory": "512MB"}}, false},
	} {
		t.Run(name, func(t *testing.T) {
			if err := create(tc.kubelet); (err == nil) != tc.admit {
				t.Errorf("admitted=%v, want %v (err: %v)", err == nil, tc.admit, err)
			}
		})
	}
}

// TestCRD_PlacementGroupAdmission checks spec.placementGroupIDs admission on a
// real API server: positive IDs only, no duplicates, and never together with
// placementGroupStrategy none, which would otherwise silently ignore one of them.
func TestCRD_PlacementGroupAdmission(t *testing.T) {
	if os.Getenv("KUBEBUILDER_ASSETS") == "" {
		t.Skip("KUBEBUILDER_ASSETS not set; run make test-envtest")
	}
	env := &envtest.Environment{
		CRDDirectoryPaths:     []string{"../../../charts/karpenter-provider-hetzner/crds"},
		ErrorIfCRDPathMissing: true,
	}
	cfg, err := env.Start()
	if err != nil {
		t.Fatalf("installing the CRDs: %v", err)
	}
	t.Cleanup(func() { _ = env.Stop() })
	c, err := client.New(cfg, client.Options{})
	if err != nil {
		t.Fatal(err)
	}

	n := 0
	for name, tc := range map[string]struct {
		spec  map[string]any
		admit bool
	}{
		"ids with default strategy": {map[string]any{"placementGroupIDs": []any{int64(1), int64(2)}}, true},
		"ids with spread":           {map[string]any{"placementGroupIDs": []any{int64(1)}, "placementGroupStrategy": "spread"}, true},
		"ids with none":             {map[string]any{"placementGroupIDs": []any{int64(1)}, "placementGroupStrategy": "none"}, false},
		"zero id":                   {map[string]any{"placementGroupIDs": []any{int64(0)}}, false},
		"duplicate ids":             {map[string]any{"placementGroupIDs": []any{int64(3), int64(3)}}, false},
		"empty list":                {map[string]any{"placementGroupIDs": []any{}}, false},
	} {
		t.Run(name, func(t *testing.T) {
			n++
			spec := map[string]any{
				"locations":     []any{"nbg1"},
				"networkID":     int64(1),
				"imageSelector": map[string]any{"family": "ubuntu"},
			}
			for k, v := range tc.spec {
				spec[k] = v
			}
			err := c.Create(context.Background(), &unstructured.Unstructured{Object: map[string]any{
				"apiVersion": "karpenter.hetzner.cloud/v1",
				"kind":       "HCloudNodeClass",
				"metadata":   map[string]any{"name": fmt.Sprintf("pg-%d", n)},
				"spec":       spec,
			}})
			if (err == nil) != tc.admit {
				t.Errorf("admitted=%v, want %v (err: %v)", err == nil, tc.admit, err)
			}
		})
	}
}

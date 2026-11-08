package operator

import (
	"testing"
	"time"
)

func TestLoadConfig_RequiresClusterName(t *testing.T) {
	t.Setenv("CLUSTER_NAME", "")
	if _, err := LoadConfig(); err == nil {
		t.Fatal("expected error when CLUSTER_NAME is unset")
	}
}

func TestLoadConfig_ReadsClusterName(t *testing.T) {
	t.Setenv("CLUSTER_NAME", "paperclip-prod")
	cfg, err := LoadConfig()
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if cfg.ClusterName != "paperclip-prod" {
		t.Errorf("got cluster name %q, want paperclip-prod", cfg.ClusterName)
	}
}

func TestLoadConfig_RejectsInvalidClusterName(t *testing.T) {
	for _, bad := range []string{"has space", "slash/name", "comma,name", "töö"} {
		t.Setenv("CLUSTER_NAME", bad)
		if _, err := LoadConfig(); err == nil {
			t.Errorf("expected error for invalid cluster name %q", bad)
		}
	}
}

func TestLoadConfig_OrphanGCDefaults(t *testing.T) {
	t.Setenv("CLUSTER_NAME", "c")
	t.Setenv("ORPHAN_GC_ENABLED", "")
	t.Setenv("ORPHAN_GC_GRACE_PERIOD", "")
	cfg, err := LoadConfig()
	if err != nil {
		t.Fatal(err)
	}
	if !cfg.OrphanGCEnabled || cfg.OrphanGCGracePeriod != defaultOrphanGracePeriod {
		t.Errorf("got enabled=%v grace=%v, want true %v", cfg.OrphanGCEnabled, cfg.OrphanGCGracePeriod, defaultOrphanGracePeriod)
	}
}

func TestLoadConfig_OrphanGCSettings(t *testing.T) {
	t.Setenv("CLUSTER_NAME", "c")
	for name, tc := range map[string]struct {
		enabled, grace string
		wantErr        bool
		wantEnabled    bool
		wantGrace      time.Duration
	}{
		"disabled":         {enabled: "false", wantEnabled: false, wantGrace: defaultOrphanGracePeriod},
		"longer grace":     {grace: "1h", wantEnabled: true, wantGrace: time.Hour},
		"minimum grace":    {grace: "15m", wantEnabled: true, wantGrace: 15 * time.Minute},
		"grace too short":  {grace: "5m", wantErr: true},
		"grace unparsable": {grace: "soon", wantErr: true},
		"enabled garbage":  {enabled: "maybe", wantErr: true},
	} {
		t.Run(name, func(t *testing.T) {
			t.Setenv("ORPHAN_GC_ENABLED", tc.enabled)
			t.Setenv("ORPHAN_GC_GRACE_PERIOD", tc.grace)
			cfg, err := LoadConfig()
			if (err != nil) != tc.wantErr {
				t.Fatalf("err = %v, wantErr %v", err, tc.wantErr)
			}
			if err == nil && (cfg.OrphanGCEnabled != tc.wantEnabled || cfg.OrphanGCGracePeriod != tc.wantGrace) {
				t.Errorf("got enabled=%v grace=%v", cfg.OrphanGCEnabled, cfg.OrphanGCGracePeriod)
			}
		})
	}
}

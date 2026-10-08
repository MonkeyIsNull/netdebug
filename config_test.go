package main

import (
	"os"
	"path/filepath"
	"testing"
)

// TestDefaultConfigLinkInterval pins the Phase-4 default: link_interval_seconds is
// 5 (slower than the 1s byte sampler) out of the box.
func TestDefaultConfigLinkInterval(t *testing.T) {
	if got := DefaultConfig().LinkIntervalSecs; got != 5 {
		t.Errorf("DefaultConfig().LinkIntervalSecs = %d, want 5", got)
	}
}

// TestLoadConfigLinkIntervalOverride confirms a config file overrides the default
// and that an absent key keeps it.
func TestLoadConfigLinkIntervalOverride(t *testing.T) {
	dir := t.TempDir()

	override := filepath.Join(dir, "cfg.json")
	if err := os.WriteFile(override, []byte(`{"link_interval_seconds":12}`), 0o644); err != nil {
		t.Fatal(err)
	}
	c, err := LoadConfig(override)
	if err != nil {
		t.Fatalf("LoadConfig: %v", err)
	}
	if c.LinkIntervalSecs != 12 {
		t.Errorf("override LinkIntervalSecs = %d, want 12", c.LinkIntervalSecs)
	}

	absent := filepath.Join(dir, "empty.json")
	if err := os.WriteFile(absent, []byte(`{}`), 0o644); err != nil {
		t.Fatal(err)
	}
	c2, err := LoadConfig(absent)
	if err != nil {
		t.Fatalf("LoadConfig: %v", err)
	}
	if c2.LinkIntervalSecs != 5 {
		t.Errorf("absent key should keep default 5, got %d", c2.LinkIntervalSecs)
	}
}

// TestDefaultConfigReachDefaults pins the Phase-7 defaults: reach_interval_seconds
// is 5 and power_gating is ON out of the box, and DefaultConfig carries BOTH public
// IPs (the two-target internet design).
func TestDefaultConfigReachDefaults(t *testing.T) {
	c := DefaultConfig()
	if c.ReachIntervalSecs != 5 {
		t.Errorf("DefaultConfig().ReachIntervalSecs = %d, want 5", c.ReachIntervalSecs)
	}
	if !c.PowerGating {
		t.Error("DefaultConfig().PowerGating = false, want true (gating ON by default)")
	}
	if len(c.PublicIPs) < 2 || c.PublicIPs[0] != "1.1.1.1" || c.PublicIPs[1] != "8.8.8.8" {
		t.Errorf("DefaultConfig().PublicIPs = %v, want [1.1.1.1 8.8.8.8]", c.PublicIPs)
	}
}

// TestLoadConfigReachOverride confirms the Phase-7 keys override the defaults and,
// CRITICALLY, that an absent power_gating key keeps the default TRUE (LoadConfig
// layers over a DefaultConfig base, so unmarshal of {} does not zero the bool).
func TestLoadConfigReachOverride(t *testing.T) {
	dir := t.TempDir()

	override := filepath.Join(dir, "cfg.json")
	if err := os.WriteFile(override, []byte(`{"reach_interval_seconds":12,"power_gating":false}`), 0o644); err != nil {
		t.Fatal(err)
	}
	c, err := LoadConfig(override)
	if err != nil {
		t.Fatalf("LoadConfig: %v", err)
	}
	if c.ReachIntervalSecs != 12 {
		t.Errorf("override ReachIntervalSecs = %d, want 12", c.ReachIntervalSecs)
	}
	if c.PowerGating {
		t.Error("explicit power_gating:false should turn gating OFF")
	}

	absent := filepath.Join(dir, "empty.json")
	if err := os.WriteFile(absent, []byte(`{}`), 0o644); err != nil {
		t.Fatal(err)
	}
	c2, err := LoadConfig(absent)
	if err != nil {
		t.Fatalf("LoadConfig: %v", err)
	}
	if c2.ReachIntervalSecs != 5 {
		t.Errorf("absent reach_interval_seconds should keep default 5, got %d", c2.ReachIntervalSecs)
	}
	if !c2.PowerGating {
		t.Error("absent power_gating must keep the default TRUE (not JSON zero-value false)")
	}
}

// TestLoadConfigSSIDLabel pins the Phase-6 display label: it loads from JSON over
// the default "", an absent key keeps "", and DefaultConfig leaves it empty (so an
// unconfigured dashboard shows the "HIDDEN (macOS-redacted)" sentinel).
func TestLoadConfigSSIDLabel(t *testing.T) {
	if got := DefaultConfig().SSIDLabel; got != "" {
		t.Errorf("DefaultConfig().SSIDLabel = %q, want empty", got)
	}

	dir := t.TempDir()
	override := filepath.Join(dir, "cfg.json")
	if err := os.WriteFile(override, []byte(`{"ssid_label":"Home-5G"}`), 0o644); err != nil {
		t.Fatal(err)
	}
	c, err := LoadConfig(override)
	if err != nil {
		t.Fatalf("LoadConfig: %v", err)
	}
	if c.SSIDLabel != "Home-5G" {
		t.Errorf("override SSIDLabel = %q, want Home-5G", c.SSIDLabel)
	}

	absent := filepath.Join(dir, "empty.json")
	if err := os.WriteFile(absent, []byte(`{}`), 0o644); err != nil {
		t.Fatal(err)
	}
	c2, err := LoadConfig(absent)
	if err != nil {
		t.Fatalf("LoadConfig: %v", err)
	}
	if c2.SSIDLabel != "" {
		t.Errorf("absent key should keep default empty, got %q", c2.SSIDLabel)
	}
}

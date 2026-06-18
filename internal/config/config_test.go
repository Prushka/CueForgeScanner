package config

import (
	"testing"
	"time"
)

func TestLoadUsesEnvTagsAndTrimsFields(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("CUEFORGE_SCAN_DIR", dir)
	t.Setenv("CUEFORGE_BASE_URL", " http://cueforge.local:8080/ ")
	t.Setenv("CUEFORGE_INPUT_LANGUAGES", "ger, eng")
	t.Setenv("CUEFORGE_TARGET_LANGUAGES", "chi, $fra")
	t.Setenv("CUEFORGE_MODEL", " model-test ")
	t.Setenv("CUEFORGE_VMODEL", " vision-test ")
	t.Setenv("CUEFORGE_REASONING_EFFORT", " medium ")
	t.Setenv("CUEFORGE_REQUEST_TIMEOUT", "90s")

	cfg, err := Load()
	if err != nil {
		t.Fatalf("Load failed: %v", err)
	}
	if cfg.ScanDir != dir {
		t.Fatalf("ScanDir = %q, want %q", cfg.ScanDir, dir)
	}
	if cfg.CueForgeBaseURL != "http://cueforge.local:8080" {
		t.Fatalf("CueForgeBaseURL = %q, want trimmed URL", cfg.CueForgeBaseURL)
	}
	if len(cfg.InputLanguages) != 2 || cfg.InputLanguages[0] != "ger" || cfg.InputLanguages[1] != "eng" {
		t.Fatalf("InputLanguages = %#v, want trimmed raw language list", cfg.InputLanguages)
	}
	if len(cfg.TargetLanguages) != 2 || cfg.TargetLanguages[0] != "chi" || cfg.TargetLanguages[1] != "$fra" {
		t.Fatalf("TargetLanguages = %#v, want trimmed raw target list", cfg.TargetLanguages)
	}
	if cfg.Model != "model-test" || cfg.VisionModel != "vision-test" || cfg.ReasoningEffort != "medium" {
		t.Fatalf("string fields not trimmed: %#v", cfg)
	}
	if cfg.RequestTimeout != 90*time.Second {
		t.Fatalf("RequestTimeout = %s, want 90s", cfg.RequestTimeout)
	}
}

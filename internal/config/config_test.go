package config

import (
	"os"
	"path/filepath"
	"slices"
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
	t.Setenv("CUEFORGE_OUTPUT_FORMATS", " srt, VTT, srt ")
	t.Setenv("CUEFORGE_REQUEST_TIMEOUT", "90s")
	t.Setenv("CUEFORGE_CONCURRENCY", "3")
	t.Setenv("CUEFORGE_SKIP_GENERATED_AFTER_UNIX", "1700000000")
	t.Setenv("CUEFORGE_SKIP_EXISTING_TARGET_FILES", "false")
	t.Setenv("SAVE_ON_ERROR", "true")
	t.Setenv("ERROR_DIR", " ~/cueforge-errors ")

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
	if !slices.Equal(cfg.OutputFormats, []string{"srt", "vtt"}) {
		t.Fatalf("OutputFormats = %#v, want trimmed deduplicated formats", cfg.OutputFormats)
	}
	if cfg.RequestTimeout != 90*time.Second {
		t.Fatalf("RequestTimeout = %s, want 90s", cfg.RequestTimeout)
	}
	if cfg.Concurrency != 3 {
		t.Fatalf("Concurrency = %d, want 3", cfg.Concurrency)
	}
	if !cfg.SaveOnError {
		t.Fatal("SaveOnError = false, want true")
	}
	if cfg.ErrorDir != filepath.Join(homeDir(t), "cueforge-errors") {
		t.Fatalf("ErrorDir = %q, want expanded ~/cueforge-errors", cfg.ErrorDir)
	}
	if !cfg.SkipGeneratedAfter.Equal(time.Unix(1_700_000_000, 0)) {
		t.Fatalf("SkipGeneratedAfter = %s, want Unix 1700000000", cfg.SkipGeneratedAfter)
	}
	if cfg.ShouldSkipExistingTargetFiles() {
		t.Fatal("ShouldSkipExistingTargetFiles = true, want false")
	}
}

func TestLoadDefaultsSkipGeneratedAfterToUnixEpoch(t *testing.T) {
	t.Setenv("CUEFORGE_INPUT_LANGUAGES", "eng")
	t.Setenv("CUEFORGE_TARGET_LANGUAGES", "jpn")
	t.Setenv("CUEFORGE_SKIP_GENERATED_AFTER_UNIX", "")
	t.Setenv("ERROR_DIR", "")
	unsetEnv(t, "CUEFORGE_SKIP_EXISTING_TARGET_FILES")
	unsetEnv(t, "SAVE_ON_ERROR")

	cfg, err := Load()
	if err != nil {
		t.Fatalf("Load failed: %v", err)
	}
	if !cfg.SkipGeneratedAfter.Equal(time.Unix(0, 0)) {
		t.Fatalf("SkipGeneratedAfter = %s, want Unix epoch", cfg.SkipGeneratedAfter)
	}
	if cfg.SaveOnError {
		t.Fatal("SaveOnError = true, want default false")
	}
	if cfg.ErrorDir != "./errors" {
		t.Fatalf("ErrorDir = %q, want ./errors", cfg.ErrorDir)
	}
	if !slices.Equal(cfg.OutputFormats, []string{"ass", "vtt", "srt"}) {
		t.Fatalf("OutputFormats = %#v, want default ass/vtt/srt", cfg.OutputFormats)
	}
	if !cfg.ShouldSkipExistingTargetFiles() {
		t.Fatal("ShouldSkipExistingTargetFiles = false, want default true")
	}
}

func TestLoadRejectsInvalidConcurrency(t *testing.T) {
	t.Setenv("CUEFORGE_INPUT_LANGUAGES", "eng")
	t.Setenv("CUEFORGE_TARGET_LANGUAGES", "jpn")
	t.Setenv("CUEFORGE_CONCURRENCY", "0")
	t.Setenv("CUEFORGE_SKIP_GENERATED_AFTER_UNIX", "")

	if _, err := Load(); err == nil {
		t.Fatal("Load succeeded, want invalid concurrency error")
	}
}

func TestLoadRejectsInvalidSkipGeneratedAfter(t *testing.T) {
	t.Setenv("CUEFORGE_INPUT_LANGUAGES", "eng")
	t.Setenv("CUEFORGE_TARGET_LANGUAGES", "jpn")
	t.Setenv("CUEFORGE_SKIP_GENERATED_AFTER_UNIX", "not-a-timestamp")

	if _, err := Load(); err == nil {
		t.Fatal("Load succeeded, want invalid skip timestamp error")
	}
}

func TestLoadParsesSaveOnErrorBool(t *testing.T) {
	for _, tt := range []struct {
		name    string
		value   string
		want    bool
		wantErr bool
	}{
		{name: "true", value: "true", want: true},
		{name: "false", value: "false"},
		{name: "on rejected", value: "on", wantErr: true},
	} {
		t.Run(tt.name, func(t *testing.T) {
			t.Setenv("CUEFORGE_INPUT_LANGUAGES", "eng")
			t.Setenv("CUEFORGE_TARGET_LANGUAGES", "jpn")
			t.Setenv("CUEFORGE_SKIP_GENERATED_AFTER_UNIX", "")
			t.Setenv("SAVE_ON_ERROR", tt.value)

			cfg, err := Load()
			if tt.wantErr {
				if err == nil {
					t.Fatal("Load succeeded, want invalid save-on-error error")
				}
				return
			}
			if err != nil {
				t.Fatalf("Load failed: %v", err)
			}
			if cfg.SaveOnError != tt.want {
				t.Fatalf("SaveOnError = %t, want %t", cfg.SaveOnError, tt.want)
			}
		})
	}
}

func TestLoadParsesSkipExistingTargetFilesBool(t *testing.T) {
	for _, tt := range []struct {
		name    string
		value   string
		want    bool
		wantErr bool
	}{
		{name: "true", value: "true", want: true},
		{name: "false", value: "false"},
		{name: "on rejected", value: "on", wantErr: true},
	} {
		t.Run(tt.name, func(t *testing.T) {
			t.Setenv("CUEFORGE_INPUT_LANGUAGES", "eng")
			t.Setenv("CUEFORGE_TARGET_LANGUAGES", "jpn")
			t.Setenv("CUEFORGE_SKIP_GENERATED_AFTER_UNIX", "")
			t.Setenv("CUEFORGE_SKIP_EXISTING_TARGET_FILES", tt.value)

			cfg, err := Load()
			if tt.wantErr {
				if err == nil {
					t.Fatal("Load succeeded, want invalid skip-existing-target-files error")
				}
				return
			}
			if err != nil {
				t.Fatalf("Load failed: %v", err)
			}
			if cfg.ShouldSkipExistingTargetFiles() != tt.want {
				t.Fatalf("ShouldSkipExistingTargetFiles = %t, want %t", cfg.ShouldSkipExistingTargetFiles(), tt.want)
			}
		})
	}
}

func TestLoadRejectsInvalidOutputFormats(t *testing.T) {
	for _, tt := range []struct {
		name  string
		value string
	}{
		{name: "empty item", value: "ass, ,srt"},
		{name: "path separator", value: "ass,../srt"},
	} {
		t.Run(tt.name, func(t *testing.T) {
			t.Setenv("CUEFORGE_INPUT_LANGUAGES", "eng")
			t.Setenv("CUEFORGE_TARGET_LANGUAGES", "jpn")
			t.Setenv("CUEFORGE_SKIP_GENERATED_AFTER_UNIX", "")
			t.Setenv("CUEFORGE_OUTPUT_FORMATS", tt.value)

			if _, err := Load(); err == nil {
				t.Fatal("Load succeeded, want invalid output formats error")
			}
		})
	}
}

func homeDir(t *testing.T) string {
	t.Helper()
	cfg, err := expandPath("~")
	if err != nil {
		t.Fatalf("expandPath(~) failed: %v", err)
	}
	return cfg
}

func unsetEnv(t *testing.T, key string) {
	t.Helper()
	old, ok := os.LookupEnv(key)
	if err := os.Unsetenv(key); err != nil {
		t.Fatalf("Unsetenv(%s) failed: %v", key, err)
	}
	t.Cleanup(func() {
		if ok {
			_ = os.Setenv(key, old)
		} else {
			_ = os.Unsetenv(key)
		}
	})
}

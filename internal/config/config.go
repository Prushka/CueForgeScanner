package config

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/caarlos0/env/v11"
)

type Config struct {
	ScanDir         string        `json:"scan_dir" env:"CUEFORGE_SCAN_DIR" envDefault:"~/GolandProjects/Sparkle/output"`
	CueForgeBaseURL string        `json:"cueforge_base_url" env:"CUEFORGE_BASE_URL" envDefault:"http://localhost:8080"`
	InputLanguages  []string      `json:"input_languages" env:"CUEFORGE_INPUT_LANGUAGES,required" envSeparator:","`
	TargetLanguages []string      `json:"target_languages" env:"CUEFORGE_TARGET_LANGUAGES,required" envSeparator:","`
	Model           string        `json:"model" env:"CUEFORGE_MODEL"`
	VisionModel     string        `json:"vision_model" env:"CUEFORGE_VMODEL"`
	ReasoningEffort string        `json:"reasoning_effort" env:"CUEFORGE_REASONING_EFFORT"`
	Media           string        `json:"media" env:"CUEFORGE_MEDIA"`
	RequestTimeout  time.Duration `json:"request_timeout" env:"CUEFORGE_REQUEST_TIMEOUT" envDefault:"0s"`
}

func Load() (Config, error) {
	cfg := Config{}
	if err := env.Parse(&cfg); err != nil {
		return Config{}, err
	}

	if err := normalize(&cfg); err != nil {
		return Config{}, err
	}
	return cfg, nil
}

func normalize(cfg *Config) error {
	var err error
	cfg.ScanDir, err = expandPath(cfg.ScanDir)
	if err != nil {
		return err
	}
	cfg.CueForgeBaseURL = strings.TrimRight(strings.TrimSpace(cfg.CueForgeBaseURL), "/")
	cfg.Model = strings.TrimSpace(cfg.Model)
	cfg.VisionModel = strings.TrimSpace(cfg.VisionModel)
	cfg.ReasoningEffort = strings.TrimSpace(cfg.ReasoningEffort)
	cfg.Media = strings.TrimSpace(cfg.Media)

	if cfg.CueForgeBaseURL == "" {
		return errors.New("CUEFORGE_BASE_URL cannot be empty")
	}
	if len(cfg.InputLanguages) == 0 {
		return errors.New("CUEFORGE_INPUT_LANGUAGES must contain at least one language")
	}
	for i, value := range cfg.InputLanguages {
		value = strings.TrimSpace(value)
		if value == "" {
			return errors.New("CUEFORGE_INPUT_LANGUAGES must contain only non-empty languages")
		}
		cfg.InputLanguages[i] = value
	}
	if len(cfg.TargetLanguages) == 0 {
		return errors.New("CUEFORGE_TARGET_LANGUAGES must contain at least one language")
	}
	for i, value := range cfg.TargetLanguages {
		value = strings.TrimSpace(value)
		if value == "" {
			return errors.New("CUEFORGE_TARGET_LANGUAGES must contain only non-empty languages")
		}
		cfg.TargetLanguages[i] = value
	}
	if cfg.RequestTimeout < 0 {
		return errors.New("CUEFORGE_REQUEST_TIMEOUT cannot be negative")
	}
	return nil
}

func expandPath(path string) (string, error) {
	path = strings.TrimSpace(path)
	if path == "" {
		return "", errors.New("path cannot be empty")
	}
	if path == "~" {
		home, err := os.UserHomeDir()
		if err != nil {
			return "", fmt.Errorf("resolve home dir: %w", err)
		}
		return home, nil
	}
	if strings.HasPrefix(path, "~/") {
		home, err := os.UserHomeDir()
		if err != nil {
			return "", fmt.Errorf("resolve home dir: %w", err)
		}
		return filepath.Join(home, path[2:]), nil
	}
	return path, nil
}

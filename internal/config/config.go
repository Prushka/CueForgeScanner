package config

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"time"

	"github.com/caarlos0/env/v11"
)

type Config struct {
	ScanDir                 string        `json:"scan_dir" env:"CUEFORGE_SCAN_DIR" envDefault:"~/GolandProjects/Sparkle/output"`
	CueForgeBaseURL         string        `json:"cueforge_base_url" env:"CUEFORGE_BASE_URL" envDefault:"http://localhost:8080"`
	InputLanguages          []string      `json:"input_languages" env:"CUEFORGE_INPUT_LANGUAGES,required" envSeparator:","`
	TargetLanguages         []string      `json:"target_languages" env:"CUEFORGE_TARGET_LANGUAGES,required" envSeparator:","`
	Model                   string        `json:"model" env:"CUEFORGE_MODEL"`
	VisionModel             string        `json:"vision_model" env:"CUEFORGE_VMODEL"`
	ReasoningEffort         string        `json:"reasoning_effort" env:"CUEFORGE_REASONING_EFFORT"`
	OutputFormats           []string      `json:"output_formats" env:"CUEFORGE_OUTPUT_FORMATS" envDefault:"ass,vtt,srt" envSeparator:","`
	RequestTimeout          time.Duration `json:"request_timeout" env:"CUEFORGE_REQUEST_TIMEOUT" envDefault:"0s"`
	Concurrency             int           `json:"concurrency" env:"CUEFORGE_CONCURRENCY" envDefault:"1"`
	SkipExistingTargetFiles *bool         `json:"skip_existing_target_files" env:"CUEFORGE_SKIP_EXISTING_TARGET_FILES"`
	SaveOnError             bool          `json:"save_on_error" env:"SAVE_ON_ERROR" envDefault:"false"`
	ErrorDir                string        `json:"error_dir" env:"ERROR_DIR" envDefault:"./errors"`
	SkipGeneratedAfter      time.Time     `json:"skip_generated_after"`
}

var outputFormatPattern = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9_-]*$`)

func DefaultOutputFormats() []string {
	return []string{"ass", "vtt", "srt"}
}

func Load() (Config, error) {
	cfg := Config{}
	if err := env.Parse(&cfg); err != nil {
		return Config{}, err
	}

	if err := parseSkipGeneratedAfter(&cfg, os.Getenv("CUEFORGE_SKIP_GENERATED_AFTER_UNIX"), time.Now()); err != nil {
		return Config{}, err
	}
	if err := normalize(&cfg); err != nil {
		return Config{}, err
	}
	return cfg, nil
}

func (cfg Config) ShouldSkipExistingTargetFiles() bool {
	return cfg.SkipExistingTargetFiles == nil || *cfg.SkipExistingTargetFiles
}

func parseSkipGeneratedAfter(cfg *Config, value string, defaultTime time.Time) error {
	value = strings.TrimSpace(value)
	if value == "" {
		cfg.SkipGeneratedAfter = defaultTime
		return nil
	}
	seconds, err := strconv.ParseInt(value, 10, 64)
	if err != nil {
		return fmt.Errorf("CUEFORGE_SKIP_GENERATED_AFTER_UNIX must be a Unix timestamp in seconds: %w", err)
	}
	if seconds < 0 {
		return errors.New("CUEFORGE_SKIP_GENERATED_AFTER_UNIX cannot be negative")
	}
	cfg.SkipGeneratedAfter = time.Unix(seconds, 0)
	return nil
}

func normalize(cfg *Config) error {
	var err error
	cfg.ScanDir, err = expandPath(cfg.ScanDir)
	if err != nil {
		return err
	}
	if strings.TrimSpace(cfg.ErrorDir) == "" {
		cfg.ErrorDir = "./errors"
	}
	cfg.ErrorDir, err = expandPath(cfg.ErrorDir)
	if err != nil {
		return err
	}
	cfg.CueForgeBaseURL = strings.TrimRight(strings.TrimSpace(cfg.CueForgeBaseURL), "/")
	cfg.Model = strings.TrimSpace(cfg.Model)
	cfg.VisionModel = strings.TrimSpace(cfg.VisionModel)
	cfg.ReasoningEffort = strings.TrimSpace(cfg.ReasoningEffort)
	cfg.OutputFormats, err = normalizeOutputFormats(cfg.OutputFormats)
	if err != nil {
		return err
	}

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
	if cfg.Concurrency < 1 {
		return errors.New("CUEFORGE_CONCURRENCY must be at least 1")
	}
	return nil
}

func (cfg Config) OutputFormatsOrDefault() []string {
	if len(cfg.OutputFormats) == 0 {
		return DefaultOutputFormats()
	}
	formats, err := normalizeOutputFormats(cfg.OutputFormats)
	if err != nil {
		return DefaultOutputFormats()
	}
	return formats
}

func normalizeOutputFormats(values []string) ([]string, error) {
	if len(values) == 0 {
		return nil, errors.New("CUEFORGE_OUTPUT_FORMATS must contain at least one format")
	}

	formats := make([]string, 0, len(values))
	seen := map[string]struct{}{}
	for _, value := range values {
		value = strings.ToLower(strings.TrimSpace(value))
		if value == "" {
			return nil, errors.New("CUEFORGE_OUTPUT_FORMATS must contain only non-empty formats")
		}
		if !outputFormatPattern.MatchString(value) {
			return nil, fmt.Errorf("CUEFORGE_OUTPUT_FORMATS contains invalid format %q", value)
		}
		if _, ok := seen[value]; ok {
			continue
		}
		seen[value] = struct{}{}
		formats = append(formats, value)
	}
	return formats, nil
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

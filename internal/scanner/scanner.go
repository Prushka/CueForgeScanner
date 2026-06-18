package scanner

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"math/rand"
	"mime/multipart"
	"net/http"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"

	"CueForgeScanner/internal/config"
	"CueForgeScanner/internal/cueforge"
)

var languageIDPattern = regexp.MustCompile(`^[A-Za-z]{3}$`)

type subtitleCandidate struct {
	Path       string
	Name       string
	LanguageID string
	Language   *cueforge.Language
}

type inputLanguage struct {
	Raw string
	IDs map[string]struct{}
}

type targetLanguage struct {
	RequestValue string
	OutputID     string
	Annotate     bool
}

type translateResponse struct {
	Outputs []translateOutput `json:"outputs"`
}

type translateOutput struct {
	Filename  string `json:"filename"`
	Format    string `json:"format"`
	Variant   string `json:"variant"`
	Annotated bool   `json:"annotated"`
	Content   string `json:"content"`
}

func Run(ctx context.Context, client *http.Client, rng *rand.Rand, cfg config.Config, languages cueforge.Registry) error {
	inputLanguages, err := resolveInputLanguages(cfg.InputLanguages, languages)
	if err != nil {
		return err
	}
	targetLanguages, err := resolveTargetLanguages(cfg.TargetLanguages, languages)
	if err != nil {
		return err
	}

	entries, err := os.ReadDir(cfg.ScanDir)
	if err != nil {
		return fmt.Errorf("read scan dir %s: %w", cfg.ScanDir, err)
	}

	var failures []error
	for _, entry := range entries {
		if !entry.IsDir() {
			continue
		}
		folder := filepath.Join(cfg.ScanDir, entry.Name())
		input, err := chooseInputSubtitle(folder, inputLanguages, languages, rng)
		if err != nil {
			if errors.Is(err, errNoASSSubtitles) {
				log.Printf("skip %s: no original .ass subtitles found", folder)
				continue
			}
			failures = append(failures, err)
			continue
		}

		log.Printf("folder %s: selected %s", entry.Name(), input.Name)
		for _, target := range targetLanguages {
			if err := translateTarget(ctx, client, cfg, folder, input, target); err != nil {
				failures = append(failures, fmt.Errorf("%s -> %s: %w", entry.Name(), target.OutputID, err))
				log.Printf("failed %s -> %s: %v", entry.Name(), target.OutputID, err)
				continue
			}
			log.Printf("folder %s: wrote cueforge_%s outputs", entry.Name(), target.OutputID)
		}
	}

	if len(failures) > 0 {
		return joinErrors(failures)
	}
	return nil
}

var errNoASSSubtitles = errors.New("no original .ass subtitles found")

func chooseInputSubtitle(folder string, priorities []inputLanguage, languages cueforge.Registry, rng *rand.Rand) (subtitleCandidate, error) {
	candidates, err := listASSCandidates(folder, languages)
	if err != nil {
		return subtitleCandidate{}, err
	}
	if len(candidates) == 0 {
		return subtitleCandidate{}, errNoASSSubtitles
	}

	for _, priority := range priorities {
		for _, candidate := range candidates {
			for id := range priority.IDs {
				if candidateMatchesLanguage(candidate, id) {
					return candidate, nil
				}
			}
		}
	}

	return candidates[rng.Intn(len(candidates))], nil
}

func listASSCandidates(folder string, languages cueforge.Registry) ([]subtitleCandidate, error) {
	entries, err := os.ReadDir(folder)
	if err != nil {
		return nil, fmt.Errorf("read folder %s: %w", folder, err)
	}

	candidates := make([]subtitleCandidate, 0)
	for _, entry := range entries {
		if entry.IsDir() {
			continue
		}
		name := entry.Name()
		lowerName := strings.ToLower(name)
		if !strings.HasSuffix(lowerName, ".ass") || strings.HasPrefix(lowerName, "cueforge_") {
			continue
		}

		langID := languageIDFromFilename(name)
		var lang *cueforge.Language
		if resolved, ok := languages.Resolve(langID); ok {
			lang = &resolved
		}
		candidates = append(candidates, subtitleCandidate{
			Path:       filepath.Join(folder, name),
			Name:       name,
			LanguageID: langID,
			Language:   lang,
		})
	}

	sort.Slice(candidates, func(i, j int) bool {
		return candidates[i].Name < candidates[j].Name
	})
	return candidates, nil
}

func translateTarget(ctx context.Context, client *http.Client, cfg config.Config, folder string, input subtitleCandidate, target targetLanguage) error {
	payload, contentType, err := buildTranslateRequest(input, target, cfg)
	if err != nil {
		return err
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, cfg.CueForgeBaseURL+"/translate", payload)
	if err != nil {
		return fmt.Errorf("create translate request: %w", err)
	}
	req.Header.Set("Content-Type", contentType)

	resp, err := client.Do(req)
	if err != nil {
		return fmt.Errorf("post translate request: %w", err)
	}
	defer resp.Body.Close()

	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return fmt.Errorf("read translate response: %w", err)
	}
	if resp.StatusCode < http.StatusOK || resp.StatusCode >= http.StatusMultipleChoices {
		return fmt.Errorf("translate returned %s: %s", resp.Status, strings.TrimSpace(string(body)))
	}

	var decoded translateResponse
	if err := json.Unmarshal(body, &decoded); err != nil {
		return fmt.Errorf("decode translate JSON: %w", err)
	}
	if len(decoded.Outputs) == 0 {
		return errors.New("translate response did not include outputs")
	}

	if err := saveTargetOutputs(folder, target, decoded.Outputs); err != nil {
		return err
	}
	return nil
}

func buildTranslateRequest(input subtitleCandidate, target targetLanguage, cfg config.Config) (io.Reader, string, error) {
	var body bytes.Buffer
	writer := multipart.NewWriter(&body)

	if err := writer.WriteField("target_language", target.RequestValue); err != nil {
		return nil, "", err
	}
	if err := writer.WriteField("response", "json"); err != nil {
		return nil, "", err
	}
	for _, format := range []string{"ass", "vtt"} {
		if err := writer.WriteField("output_format", format); err != nil {
			return nil, "", err
		}
	}
	if target.Annotate {
		if err := writer.WriteField("annotate", "true"); err != nil {
			return nil, "", err
		}
	}
	if input.Language != nil && input.LanguageID != "" {
		if err := writer.WriteField("input_language", input.LanguageID); err != nil {
			return nil, "", err
		}
	}
	if cfg.Model != "" {
		if err := writer.WriteField("model", cfg.Model); err != nil {
			return nil, "", err
		}
	}
	if cfg.VisionModel != "" {
		if err := writer.WriteField("vmodel", cfg.VisionModel); err != nil {
			return nil, "", err
		}
	}
	if cfg.ReasoningEffort != "" {
		if err := writer.WriteField("reasoning_effort", cfg.ReasoningEffort); err != nil {
			return nil, "", err
		}
	}
	if cfg.Media != "" {
		if err := writer.WriteField("media", cfg.Media); err != nil {
			return nil, "", err
		}
	}

	file, err := os.Open(input.Path)
	if err != nil {
		return nil, "", fmt.Errorf("open input subtitle %s: %w", input.Path, err)
	}
	defer file.Close()

	part, err := writer.CreateFormFile("file", input.Name)
	if err != nil {
		return nil, "", fmt.Errorf("create file part: %w", err)
	}
	if _, err := io.Copy(part, file); err != nil {
		return nil, "", fmt.Errorf("write file part: %w", err)
	}

	if err := writer.Close(); err != nil {
		return nil, "", fmt.Errorf("close multipart writer: %w", err)
	}
	return &body, writer.FormDataContentType(), nil
}

func saveTargetOutputs(folder string, target targetLanguage, outputs []translateOutput) error {
	found := map[string]bool{}
	for _, output := range outputs {
		format := strings.ToLower(strings.TrimSpace(output.Format))
		if format != "ass" && format != "vtt" {
			continue
		}

		var name string
		if output.Annotated || output.Variant == "annotated" {
			name = fmt.Sprintf("cueforge_%s_annotated.%s", target.OutputID, format)
			found["annotated:"+format] = true
		} else if output.Variant == "" || output.Variant == "translated" {
			name = fmt.Sprintf("cueforge_%s.%s", target.OutputID, format)
			found["translated:"+format] = true
		} else {
			continue
		}

		if err := os.WriteFile(filepath.Join(folder, name), []byte(output.Content), 0o644); err != nil {
			return fmt.Errorf("write %s: %w", name, err)
		}
	}

	for _, format := range []string{"ass", "vtt"} {
		if !found["translated:"+format] {
			return fmt.Errorf("translate response missing translated %s output", format)
		}
		if target.Annotate && !found["annotated:"+format] {
			return fmt.Errorf("translate response missing annotated %s output", format)
		}
	}
	return nil
}

func resolveInputLanguages(values []string, languages cueforge.Registry) ([]inputLanguage, error) {
	out := make([]inputLanguage, 0, len(values))
	for _, value := range values {
		ids, err := languages.IDSet(value)
		if err != nil {
			return nil, fmt.Errorf("input language %q: %w", value, err)
		}
		out = append(out, inputLanguage{Raw: value, IDs: ids})
	}
	return out, nil
}

func resolveTargetLanguages(values []string, languages cueforge.Registry) ([]targetLanguage, error) {
	out := make([]targetLanguage, 0, len(values))
	for _, value := range values {
		raw := strings.TrimSpace(value)
		annotate := false
		languageValue := raw
		if strings.HasPrefix(languageValue, "$") {
			annotate = true
			languageValue = strings.TrimSpace(strings.TrimPrefix(languageValue, "$"))
		}
		if languageValue == "" {
			return nil, fmt.Errorf("target language %q is empty after annotation prefix", raw)
		}

		outputID, err := languages.ConfiguredID(languageValue)
		if err != nil {
			return nil, fmt.Errorf("target language %q: %w", languageValue, err)
		}
		out = append(out, targetLanguage{
			RequestValue: languageValue,
			OutputID:     outputID,
			Annotate:     annotate,
		})
	}
	return out, nil
}

func candidateMatchesLanguage(candidate subtitleCandidate, id string) bool {
	id = strings.ToLower(id)
	if strings.ToLower(candidate.LanguageID) == id {
		return true
	}
	if candidate.Language == nil {
		return false
	}
	for _, candidateID := range candidate.Language.IDs {
		if strings.ToLower(candidateID) == id {
			return true
		}
	}
	return false
}

func languageIDFromFilename(name string) string {
	stem := strings.TrimSuffix(filepath.Base(name), filepath.Ext(name))
	idx := strings.LastIndex(stem, "-")
	if idx < 0 || idx == len(stem)-1 {
		return ""
	}
	id := strings.ToLower(stem[idx+1:])
	if !languageIDPattern.MatchString(id) {
		return ""
	}
	return id
}

func joinErrors(errs []error) error {
	if len(errs) == 1 {
		return errs[0]
	}
	var b strings.Builder
	fmt.Fprintf(&b, "%d failures:", len(errs))
	for _, err := range errs {
		fmt.Fprintf(&b, "\n- %v", err)
	}
	return errors.New(b.String())
}

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
	"sync"
	"time"

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

type jobMetadata struct {
	Media *jobMediaMetadata `json:"media"`
}

type jobMediaMetadata struct {
	Title string `json:"title"`
}

type scanFolder struct {
	Name    string
	Path    string
	ModTime time.Time
}

type folderTask struct {
	Index  int
	Folder scanFolder
}

type folderResult struct {
	Index  int
	Errors []error
}

type lockedRand struct {
	mu  sync.Mutex
	rng *rand.Rand
}

func newLockedRand(rng *rand.Rand) *lockedRand {
	return &lockedRand{rng: rng}
}

func (r *lockedRand) Intn(n int) int {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.rng.Intn(n)
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

	folders, err := listScanFolders(cfg.ScanDir)
	if err != nil {
		return err
	}

	results := processFolders(ctx, client, newLockedRand(rng), cfg, languages, inputLanguages, targetLanguages, folders)

	var failures []error
	for _, result := range results {
		failures = append(failures, result.Errors...)
	}

	if len(failures) > 0 {
		return joinErrors(failures)
	}
	return nil
}

var errNoASSSubtitles = errors.New("no original .ass subtitles found")

func processFolders(ctx context.Context, client *http.Client, rng *lockedRand, cfg config.Config, languages cueforge.Registry, inputLanguages []inputLanguage, targetLanguages []targetLanguage, folders []scanFolder) []folderResult {
	if len(folders) == 0 {
		return nil
	}

	workerCount := cfg.Concurrency
	if workerCount < 1 {
		workerCount = 1
	}
	if workerCount > len(folders) {
		workerCount = len(folders)
	}

	tasks := make(chan folderTask)
	results := make(chan folderResult, len(folders))
	var wg sync.WaitGroup
	for worker := 0; worker < workerCount; worker++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for task := range tasks {
				results <- folderResult{
					Index:  task.Index,
					Errors: processFolder(ctx, client, rng, cfg, languages, inputLanguages, targetLanguages, task.Folder),
				}
			}
		}()
	}

	for i, folder := range folders {
		tasks <- folderTask{Index: i, Folder: folder}
	}
	close(tasks)
	wg.Wait()
	close(results)

	out := make([]folderResult, 0, len(folders))
	for result := range results {
		out = append(out, result)
	}
	sort.Slice(out, func(i, j int) bool {
		return out[i].Index < out[j].Index
	})
	return out
}

func processFolder(ctx context.Context, client *http.Client, rng *lockedRand, cfg config.Config, languages cueforge.Registry, inputLanguages []inputLanguage, targetLanguages []targetLanguage, folder scanFolder) []error {
	input, err := chooseInputSubtitle(folder.Path, inputLanguages, languages, rng)
	if err != nil {
		if errors.Is(err, errNoASSSubtitles) {
			log.Printf("skip %s: no original .ass subtitles found", folder.Path)
			return nil
		}
		return []error{err}
	}

	var failures []error
	mediaTitle := mediaTitleFromJob(folder.Path)
	if mediaTitle != "" {
		log.Printf("folder %s: selected input=%s input_language=%s media=%q targets=%d", folder.Name, input.Name, inputLanguageLogValue(input), mediaTitle, len(targetLanguages))
	} else {
		log.Printf("folder %s: selected input=%s input_language=%s targets=%d", folder.Name, input.Name, inputLanguageLogValue(input), len(targetLanguages))
	}
	for _, target := range targetLanguages {
		targetFields := targetLogFields(target)
		log.Printf("folder %s: translating input=%s input_language=%s %s", folder.Name, input.Name, inputLanguageLogValue(input), targetFields)
		if err := translateTarget(ctx, client, cfg, folder.Path, mediaTitle, input, target); err != nil {
			failures = append(failures, fmt.Errorf("%s -> %s: %w", folder.Name, target.OutputID, err))
			log.Printf("folder %s: failed input=%s %s: %v", folder.Name, input.Name, targetFields, err)
			continue
		}
		log.Printf("folder %s: wrote outputs %s files=%s", folder.Name, targetFields, strings.Join(outputFileNames(target), ","))
	}
	if len(failures) > 0 {
		return failures
	}
	return nil
}

func listScanFolders(scanDir string) ([]scanFolder, error) {
	entries, err := os.ReadDir(scanDir)
	if err != nil {
		return nil, fmt.Errorf("read scan dir %s: %w", scanDir, err)
	}

	folders := make([]scanFolder, 0, len(entries))
	for _, entry := range entries {
		if !entry.IsDir() {
			continue
		}
		info, err := entry.Info()
		if err != nil {
			return nil, fmt.Errorf("stat scan folder %s: %w", filepath.Join(scanDir, entry.Name()), err)
		}
		folders = append(folders, scanFolder{
			Name:    entry.Name(),
			Path:    filepath.Join(scanDir, entry.Name()),
			ModTime: info.ModTime(),
		})
	}

	sort.Slice(folders, func(i, j int) bool {
		if folders[i].ModTime.Equal(folders[j].ModTime) {
			return folders[i].Name < folders[j].Name
		}
		return folders[i].ModTime.After(folders[j].ModTime)
	})
	return folders, nil
}

func inputLanguageLogValue(input subtitleCandidate) string {
	if input.LanguageID == "" {
		return "unknown"
	}
	if input.Language == nil {
		return input.LanguageID + " (unrecognized)"
	}
	return input.LanguageID
}

func targetLogFields(target targetLanguage) string {
	annotation := "off"
	if target.Annotate {
		annotation = "on"
	}
	if target.RequestValue == target.OutputID {
		return fmt.Sprintf("target=%s annotation=%s", target.OutputID, annotation)
	}
	return fmt.Sprintf("target=%s request_target=%q annotation=%s", target.OutputID, target.RequestValue, annotation)
}

func outputFileNames(target targetLanguage) []string {
	names := []string{
		fmt.Sprintf("cueforge_%s.ass", target.OutputID),
		fmt.Sprintf("cueforge_%s.vtt", target.OutputID),
	}
	if target.Annotate {
		names = append(names,
			fmt.Sprintf("cueforge_%s_annotated.ass", target.OutputID),
			fmt.Sprintf("cueforge_%s_annotated.vtt", target.OutputID),
		)
	}
	return names
}

func chooseInputSubtitle(folder string, priorities []inputLanguage, languages cueforge.Registry, rng interface{ Intn(int) int }) (subtitleCandidate, error) {
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

func translateTarget(ctx context.Context, client *http.Client, cfg config.Config, folder, mediaTitle string, input subtitleCandidate, target targetLanguage) error {
	payload, contentType, err := buildTranslateRequest(input, target, cfg, mediaTitle)
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

func buildTranslateRequest(input subtitleCandidate, target targetLanguage, cfg config.Config, mediaTitle string) (io.Reader, string, error) {
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
	if mediaTitle != "" {
		if err := writer.WriteField("media", mediaTitle); err != nil {
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

func mediaTitleFromJob(folder string) string {
	data, err := os.ReadFile(filepath.Join(folder, "job.json"))
	if err != nil {
		return ""
	}
	var job jobMetadata
	if err := json.Unmarshal(data, &job); err != nil {
		return ""
	}
	if job.Media == nil {
		return ""
	}
	return strings.TrimSpace(job.Media.Title)
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

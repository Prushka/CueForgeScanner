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

var supportedInputSubtitleExtensions = map[string]int{
	".ass": 0,
	".vtt": 1,
	".sup": 2,
	".sub": 3,
}

type subtitleCandidate struct {
	Path       string
	Name       string
	LanguageID string
	Language   *cueforge.Language
	IdxPath    string
	IdxName    string
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
	Filename    string `json:"filename"`
	Format      string `json:"format"`
	Variant     string `json:"variant"`
	Annotated   bool   `json:"annotated"`
	OCROriginal bool   `json:"ocr_original"`
	Content     string `json:"content"`
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

type keyedLocks struct {
	mu    sync.Mutex
	locks map[string]*sync.Mutex
}

type folderLocks struct {
	files           *keyedLocks
	outputLanguages *keyedLocks
	sharedMu        sync.Mutex
	sharedWrites    map[string]struct{}
}

type translationLimiter chan struct{}

func newLockedRand(rng *rand.Rand) *lockedRand {
	return &lockedRand{rng: rng}
}

func (r *lockedRand) Intn(n int) int {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.rng.Intn(n)
}

func newKeyedLocks() *keyedLocks {
	return &keyedLocks{locks: map[string]*sync.Mutex{}}
}

func (l *keyedLocks) lock(key string) func() {
	l.mu.Lock()
	mu, ok := l.locks[key]
	if !ok {
		mu = &sync.Mutex{}
		l.locks[key] = mu
	}
	l.mu.Unlock()

	mu.Lock()
	return mu.Unlock
}

func newFolderLocks() *folderLocks {
	return &folderLocks{
		files:           newKeyedLocks(),
		outputLanguages: newKeyedLocks(),
		sharedWrites:    map[string]struct{}{},
	}
}

func lockFile(locks *folderLocks, path string) func() {
	if locks == nil {
		return func() {}
	}
	return locks.files.lock(filepath.Clean(path))
}

func lockOutputLanguage(locks *folderLocks, outputID string) func() {
	if locks == nil {
		return func() {}
	}
	return locks.outputLanguages.lock(strings.ToLower(outputID))
}

func claimSharedWrite(locks *folderLocks, path string) bool {
	if locks == nil {
		return true
	}
	key := filepath.Clean(path)
	locks.sharedMu.Lock()
	defer locks.sharedMu.Unlock()
	if _, ok := locks.sharedWrites[key]; ok {
		return false
	}
	locks.sharedWrites[key] = struct{}{}
	return true
}

func releaseSharedWrite(locks *folderLocks, path string) {
	if locks == nil {
		return
	}
	key := filepath.Clean(path)
	locks.sharedMu.Lock()
	delete(locks.sharedWrites, key)
	locks.sharedMu.Unlock()
}

func newTranslationLimiter(concurrency int) translationLimiter {
	if concurrency < 1 {
		concurrency = 1
	}
	return make(translationLimiter, concurrency)
}

func (l translationLimiter) acquire(ctx context.Context) (func(), error) {
	if l == nil {
		return func() {}, nil
	}
	select {
	case l <- struct{}{}:
		return func() { <-l }, nil
	case <-ctx.Done():
		return nil, ctx.Err()
	}
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

var errNoSupportedSubtitles = errors.New("no supported original subtitles found")

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
	limiter := newTranslationLimiter(cfg.Concurrency)
	var wg sync.WaitGroup
	for worker := 0; worker < workerCount; worker++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for task := range tasks {
				results <- folderResult{
					Index:  task.Index,
					Errors: processFolder(ctx, client, rng, cfg, languages, inputLanguages, targetLanguages, task.Folder, limiter),
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

func processFolder(ctx context.Context, client *http.Client, rng *lockedRand, cfg config.Config, languages cueforge.Registry, inputLanguages []inputLanguage, targetLanguages []targetLanguage, folder scanFolder, limiter translationLimiter) []error {
	locks := newFolderLocks()
	input, err := chooseInputSubtitle(folder.Path, inputLanguages, languages, rng)
	if err != nil {
		if errors.Is(err, errNoSupportedSubtitles) {
			log.Printf("skip %s: no supported original subtitles found", folder.Path)
			return nil
		}
		return []error{err}
	}

	var failures []error
	mediaTitle := mediaTitleFromJob(folder.Path, locks)
	if mediaTitle != "" {
		log.Printf("folder %s: selected input=%s input_language=%s media=%q targets=%d", folder.Name, input.Name, inputLanguageLogValue(input), mediaTitle, len(targetLanguages))
	} else {
		log.Printf("folder %s: selected input=%s input_language=%s targets=%d", folder.Name, input.Name, inputLanguageLogValue(input), len(targetLanguages))
	}
	results := processTargets(ctx, client, cfg, locks, folder, mediaTitle, input, targetLanguages, limiter)
	for _, result := range results {
		if result.Err != nil {
			failures = append(failures, result.Err)
		}
	}
	if len(failures) > 0 {
		return failures
	}
	return nil
}

type targetResult struct {
	Index int
	Err   error
}

func processTargets(ctx context.Context, client *http.Client, cfg config.Config, locks *folderLocks, folder scanFolder, mediaTitle string, input subtitleCandidate, targetLanguages []targetLanguage, limiter translationLimiter) []targetResult {
	if len(targetLanguages) == 0 {
		return nil
	}

	results := make(chan targetResult, len(targetLanguages))
	var wg sync.WaitGroup
	for i, target := range targetLanguages {
		wg.Add(1)
		go func(index int, target targetLanguage) {
			defer wg.Done()
			results <- targetResult{
				Index: index,
				Err:   processTarget(ctx, client, cfg, locks, folder, mediaTitle, input, target, limiter),
			}
		}(i, target)
	}
	wg.Wait()
	close(results)

	out := make([]targetResult, 0, len(targetLanguages))
	for result := range results {
		out = append(out, result)
	}
	sort.Slice(out, func(i, j int) bool {
		return out[i].Index < out[j].Index
	})
	return out
}

func processTarget(ctx context.Context, client *http.Client, cfg config.Config, locks *folderLocks, folder scanFolder, mediaTitle string, input subtitleCandidate, target targetLanguage, limiter translationLimiter) error {
	unlockTarget := lockOutputLanguage(locks, target.OutputID)
	defer unlockTarget()

	targetFields := targetLogFields(target)
	expectedFiles := expectedOutputFileNames(input, target)
	if allOutputFilesExist(folder.Path, expectedFiles, locks) {
		log.Printf("folder %s: skipping existing outputs %s files=%s", folder.Name, targetFields, strings.Join(expectedFiles, ","))
		return nil
	}
	log.Printf("folder %s: translating input=%s input_language=%s %s", folder.Name, input.Name, inputLanguageLogValue(input), targetFields)
	release, err := limiter.acquire(ctx)
	if err != nil {
		return fmt.Errorf("%s -> %s: acquire translation slot: %w", folder.Name, target.OutputID, err)
	}
	defer release()
	if err := translateTarget(ctx, client, cfg, locks, folder.Path, mediaTitle, input, target); err != nil {
		err = fmt.Errorf("%s -> %s: %w", folder.Name, target.OutputID, err)
		log.Printf("folder %s: failed input=%s %s: %v", folder.Name, input.Name, targetFields, err)
		return err
	}
	log.Printf("folder %s: wrote outputs %s files=%s", folder.Name, targetFields, strings.Join(expectedFiles, ","))
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

func expectedOutputFileNames(input subtitleCandidate, target targetLanguage) []string {
	var names []string
	names = appendUnique(names,
		fmt.Sprintf("cueforge_%s.ass", target.OutputID),
		fmt.Sprintf("cueforge_%s.vtt", target.OutputID),
	)
	if target.Annotate {
		names = appendUnique(names,
			fmt.Sprintf("cueforge_%s_annotated.ass", target.OutputID),
			fmt.Sprintf("cueforge_%s_annotated.vtt", target.OutputID),
		)
	}
	if inputNeedsOCROriginalOutputs(input) {
		names = appendUnique(names,
			fmt.Sprintf("cueforge_%s.ass", input.LanguageID),
			fmt.Sprintf("cueforge_%s.vtt", input.LanguageID),
		)
	}
	return names
}

func appendUnique(values []string, next ...string) []string {
	for _, value := range next {
		found := false
		for _, existing := range values {
			if existing == value {
				found = true
				break
			}
		}
		if !found {
			values = append(values, value)
		}
	}
	return values
}

func allOutputFilesExist(folder string, names []string, locks *folderLocks) bool {
	for _, name := range names {
		path := filepath.Join(folder, name)
		unlock := lockFile(locks, path)
		_, err := os.Stat(path)
		unlock()
		if err != nil {
			return false
		}
	}
	return true
}

func inputNeedsOCROriginalOutputs(input subtitleCandidate) bool {
	switch strings.ToLower(filepath.Ext(input.Name)) {
	case ".sup":
		return input.LanguageID != ""
	case ".sub":
		return input.LanguageID != "" && input.IdxPath != ""
	default:
		return false
	}
}

func matchingVobSubIndexPath(inputPath string) string {
	return strings.TrimSuffix(inputPath, filepath.Ext(inputPath)) + ".idx"
}

func chooseInputSubtitle(folder string, priorities []inputLanguage, languages cueforge.Registry, rng interface{ Intn(int) int }) (subtitleCandidate, error) {
	candidates, err := listSubtitleCandidates(folder, languages)
	if err != nil {
		return subtitleCandidate{}, err
	}
	if len(candidates) == 0 {
		return subtitleCandidate{}, errNoSupportedSubtitles
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

func listSubtitleCandidates(folder string, languages cueforge.Registry) ([]subtitleCandidate, error) {
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
		ext := strings.ToLower(filepath.Ext(name))
		if _, ok := supportedInputSubtitleExtensions[ext]; !ok || strings.HasPrefix(strings.ToLower(name), "cueforge_") {
			continue
		}

		langID := languageIDFromFilename(name)
		var lang *cueforge.Language
		if resolved, ok := languages.Resolve(langID); ok {
			lang = &resolved
		}
		candidate := subtitleCandidate{
			Path:       filepath.Join(folder, name),
			Name:       name,
			LanguageID: langID,
			Language:   lang,
		}
		if ext == ".sub" {
			idxPath := matchingVobSubIndexPath(candidate.Path)
			if _, err := os.Stat(idxPath); err == nil {
				candidate.IdxPath = idxPath
				candidate.IdxName = filepath.Base(idxPath)
			}
		}
		candidates = append(candidates, candidate)
	}

	sort.Slice(candidates, func(i, j int) bool {
		iExt := supportedInputSubtitleExtensions[strings.ToLower(filepath.Ext(candidates[i].Name))]
		jExt := supportedInputSubtitleExtensions[strings.ToLower(filepath.Ext(candidates[j].Name))]
		if iExt != jExt {
			return iExt < jExt
		}
		return candidates[i].Name < candidates[j].Name
	})
	return candidates, nil
}

func translateTarget(ctx context.Context, client *http.Client, cfg config.Config, locks *folderLocks, folder, mediaTitle string, input subtitleCandidate, target targetLanguage) error {
	payload, contentType, err := buildTranslateRequest(input, target, cfg, locks, mediaTitle)
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

	if err := saveTargetOutputs(folder, input, target, decoded.Outputs, locks); err != nil {
		return err
	}
	return nil
}

func buildTranslateRequest(input subtitleCandidate, target targetLanguage, cfg config.Config, locks *folderLocks, mediaTitle string) (io.Reader, string, error) {
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

	if err := writeFormFileFromPath(writer, "file", input.Path, input.Name, locks); err != nil {
		return nil, "", err
	}

	if input.IdxPath != "" {
		if err := writeFormFileFromPath(writer, "idx", input.IdxPath, input.IdxName, locks); err != nil {
			return nil, "", err
		}
	}

	if err := writer.Close(); err != nil {
		return nil, "", fmt.Errorf("close multipart writer: %w", err)
	}
	return &body, writer.FormDataContentType(), nil
}

func writeFormFileFromPath(writer *multipart.Writer, field, path, name string, locks *folderLocks) error {
	part, err := writer.CreateFormFile(field, name)
	if err != nil {
		return fmt.Errorf("create %s part: %w", field, err)
	}

	unlock := lockFile(locks, path)
	defer unlock()

	file, err := os.Open(path)
	if err != nil {
		return fmt.Errorf("open %s %s: %w", field, path, err)
	}
	defer file.Close()

	if _, err := io.Copy(part, file); err != nil {
		return fmt.Errorf("write %s part: %w", field, err)
	}
	return nil
}

func saveTargetOutputs(folder string, input subtitleCandidate, target targetLanguage, outputs []translateOutput, locks *folderLocks) error {
	found := map[string]bool{}
	for _, output := range outputs {
		format := strings.ToLower(strings.TrimSpace(output.Format))
		if format != "ass" && format != "vtt" {
			continue
		}

		var name string
		writeOncePerRun := false
		switch {
		case output.OCROriginal || output.Variant == "ocr_original":
			if input.LanguageID == "" {
				continue
			}
			name = fmt.Sprintf("cueforge_%s.%s", input.LanguageID, format)
			found["ocr:"+format] = true
			writeOncePerRun = true
		case output.Annotated || output.Variant == "annotated":
			name = fmt.Sprintf("cueforge_%s_annotated.%s", target.OutputID, format)
			found["annotated:"+format] = true
		case output.Variant == "" || output.Variant == "translated":
			name = fmt.Sprintf("cueforge_%s.%s", target.OutputID, format)
			found["translated:"+format] = true
		default:
			continue
		}

		if err := writeOutputFile(filepath.Join(folder, name), output.Content, locks, writeOncePerRun); err != nil {
			return fmt.Errorf("write %s: %w", name, err)
		}
	}

	for _, format := range []string{"ass", "vtt"} {
		if inputNeedsOCROriginalOutputs(input) && !found["ocr:"+format] {
			return fmt.Errorf("translate response missing OCR original %s output", format)
		}
		if !found["translated:"+format] {
			return fmt.Errorf("translate response missing translated %s output", format)
		}
		if target.Annotate && !found["annotated:"+format] {
			return fmt.Errorf("translate response missing annotated %s output", format)
		}
	}
	return nil
}

func writeOutputFile(path, content string, locks *folderLocks, writeOncePerRun bool) (err error) {
	if writeOncePerRun {
		if !claimSharedWrite(locks, path) {
			return nil
		}
		defer func() {
			if err != nil {
				releaseSharedWrite(locks, path)
			}
		}()
	}

	unlock := lockFile(locks, path)
	defer unlock()

	tmp, err := os.CreateTemp(filepath.Dir(path), "."+filepath.Base(path)+".tmp-*")
	if err != nil {
		return fmt.Errorf("create temp output: %w", err)
	}
	tmpName := tmp.Name()
	removeTemp := true
	defer func() {
		if removeTemp {
			_ = os.Remove(tmpName)
		}
	}()

	if _, err := io.WriteString(tmp, content); err != nil {
		_ = tmp.Close()
		return fmt.Errorf("write temp output: %w", err)
	}
	if err := tmp.Chmod(0o644); err != nil {
		_ = tmp.Close()
		return fmt.Errorf("chmod temp output: %w", err)
	}
	if err := tmp.Close(); err != nil {
		return fmt.Errorf("close temp output: %w", err)
	}
	if err := os.Rename(tmpName, path); err != nil {
		return fmt.Errorf("rename temp output: %w", err)
	}
	removeTemp = false
	return nil
}

func mediaTitleFromJob(folder string, locks *folderLocks) string {
	path := filepath.Join(folder, "job.json")
	unlock := lockFile(locks, path)
	data, err := os.ReadFile(path)
	unlock()
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

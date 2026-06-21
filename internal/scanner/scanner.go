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
	Size       int64
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

type translateRequestParameters struct {
	TargetLanguage  string   `json:"target_language"`
	Response        string   `json:"response"`
	OutputFormat    []string `json:"output_format"`
	Annotate        bool     `json:"annotate"`
	InputLanguage   string   `json:"input_language,omitempty"`
	Model           string   `json:"model,omitempty"`
	VisionModel     string   `json:"vmodel,omitempty"`
	ReasoningEffort string   `json:"reasoning_effort,omitempty"`
	Media           string   `json:"media,omitempty"`
}

type translateRequestField struct {
	Name  string
	Value string
}

type failedSubtitleReport struct {
	SourceFilename    string                     `json:"source_filename"`
	SourcePath        string                     `json:"source_path,omitempty"`
	IdxFilename       string                     `json:"idx_filename,omitempty"`
	IdxPath           string                     `json:"idx_path,omitempty"`
	MediaTitle        string                     `json:"media_title,omitempty"`
	OriginalFolder    string                     `json:"original_folder"`
	TargetLanguage    string                     `json:"target_language"`
	Annotate          bool                       `json:"annotate"`
	RequestParameters translateRequestParameters `json:"request_parameters"`
	ErrorResponse     failedTranslateResponse    `json:"error_response"`
}

type failedTranslateResponse struct {
	StatusCode int    `json:"status_code,omitempty"`
	Status     string `json:"status,omitempty"`
	Body       string `json:"body,omitempty"`
	Error      string `json:"error,omitempty"`
}

type outputSaveMode int

const (
	saveAllOutputs outputSaveMode = iota
	saveAnnotatedOnlyOutputs
)

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
	cfg = withDefaultSkipGeneratedAfter(cfg)

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
	cfg = withDefaultSkipGeneratedAfter(cfg)

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
	results := processTargets(ctx, client, cfg, languages, locks, folder, mediaTitle, input, targetLanguages, limiter)
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

func withDefaultSkipGeneratedAfter(cfg config.Config) config.Config {
	if cfg.SkipGeneratedAfter.IsZero() {
		cfg.SkipGeneratedAfter = time.Now()
	}
	return cfg
}

type targetResult struct {
	Index int
	Err   error
}

func processTargets(ctx context.Context, client *http.Client, cfg config.Config, languages cueforge.Registry, locks *folderLocks, folder scanFolder, mediaTitle string, input subtitleCandidate, targetLanguages []targetLanguage, limiter translationLimiter) []targetResult {
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
				Err:   processTarget(ctx, client, cfg, languages, locks, folder, mediaTitle, input, target, limiter),
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

func processTarget(ctx context.Context, client *http.Client, cfg config.Config, languages cueforge.Registry, locks *folderLocks, folder scanFolder, mediaTitle string, input subtitleCandidate, target targetLanguage, limiter translationLimiter) error {
	unlockTarget := lockOutputLanguage(locks, target.OutputID)
	defer unlockTarget()

	targetFields := targetLogFields(target)
	if !inputNeedsOCROriginalOutputs(input) {
		existingTargetInput, ok, err := chooseExistingTargetTextSubtitle(folder.Path, target, languages)
		if err != nil {
			return fmt.Errorf("%s -> %s: find existing target subtitle: %w", folder.Name, target.OutputID, err)
		}
		if ok {
			if !target.Annotate {
				log.Printf("folder %s: skipping existing target-language subtitle input=%s %s", folder.Name, existingTargetInput.Name, targetFields)
				return nil
			}

			expectedFiles := annotatedOutputFileNames(target)
			if allOutputFilesExistAfter(folder.Path, expectedFiles, cfg.SkipGeneratedAfter, locks) {
				log.Printf("folder %s: skipping existing annotated outputs %s generated_after=%s files=%s", folder.Name, targetFields, cfg.SkipGeneratedAfter.Format(time.RFC3339), strings.Join(expectedFiles, ","))
				return nil
			}
			log.Printf("folder %s: annotating existing target-language input=%s input_language=%s %s", folder.Name, existingTargetInput.Name, inputLanguageLogValue(existingTargetInput), targetFields)
			release, err := limiter.acquire(ctx)
			if err != nil {
				return fmt.Errorf("%s -> %s: acquire translation slot: %w", folder.Name, target.OutputID, err)
			}
			defer release()
			if err := translateTargetWithSaveMode(ctx, client, cfg, locks, folder.Path, mediaTitle, existingTargetInput, target, saveAnnotatedOnlyOutputs); err != nil {
				err = fmt.Errorf("%s -> %s: %w", folder.Name, target.OutputID, err)
				log.Printf("folder %s: failed input=%s %s: %v", folder.Name, existingTargetInput.Name, targetFields, err)
				return err
			}
			log.Printf("folder %s: wrote annotated outputs %s files=%s", folder.Name, targetFields, strings.Join(expectedFiles, ","))
			return nil
		}
	}

	expectedFiles := expectedOutputFileNames(input, target)
	if allOutputFilesExistAfter(folder.Path, expectedFiles, cfg.SkipGeneratedAfter, locks) {
		log.Printf("folder %s: skipping existing outputs %s generated_after=%s files=%s", folder.Name, targetFields, cfg.SkipGeneratedAfter.Format(time.RFC3339), strings.Join(expectedFiles, ","))
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
		names = appendUnique(names, annotatedOutputFileNames(target)...)
	}
	if inputNeedsOCROriginalOutputs(input) {
		names = appendUnique(names,
			fmt.Sprintf("cueforge_%s.ass", input.LanguageID),
			fmt.Sprintf("cueforge_%s.vtt", input.LanguageID),
		)
	}
	return names
}

func annotatedOutputFileNames(target targetLanguage) []string {
	return []string{
		fmt.Sprintf("cueforge_%s_annotated.ass", target.OutputID),
		fmt.Sprintf("cueforge_%s_annotated.vtt", target.OutputID),
	}
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

func allOutputFilesExistAfter(folder string, names []string, cutoff time.Time, locks *folderLocks) bool {
	for _, name := range names {
		path := filepath.Join(folder, name)
		unlock := lockFile(locks, path)
		info, err := os.Stat(path)
		unlock()
		if err != nil {
			return false
		}
		if !info.ModTime().After(cutoff) {
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
		info, err := entry.Info()
		if err != nil {
			return nil, fmt.Errorf("stat subtitle candidate %s: %w", filepath.Join(folder, name), err)
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
			Size:       info.Size(),
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
		iLang := candidateLanguageSortKey(candidates[i])
		jLang := candidateLanguageSortKey(candidates[j])
		if iLang != jLang {
			return iLang < jLang
		}
		if candidates[i].Size != candidates[j].Size {
			return candidates[i].Size > candidates[j].Size
		}
		return candidates[i].Name < candidates[j].Name
	})
	return candidates, nil
}

func candidateLanguageSortKey(candidate subtitleCandidate) string {
	if candidate.Language != nil && len(candidate.Language.IDs) > 0 {
		return strings.ToLower(candidate.Language.IDs[0])
	}
	return strings.ToLower(candidate.LanguageID)
}

func chooseExistingTargetTextSubtitle(folder string, target targetLanguage, languages cueforge.Registry) (subtitleCandidate, bool, error) {
	entries, err := os.ReadDir(folder)
	if err != nil {
		return subtitleCandidate{}, false, fmt.Errorf("read folder %s: %w", folder, err)
	}

	ids, err := languages.IDSet(target.OutputID)
	if err != nil {
		return subtitleCandidate{}, false, err
	}
	ids[strings.ToLower(target.OutputID)] = struct{}{}

	candidates := make([]subtitleCandidate, 0)
	for _, entry := range entries {
		if entry.IsDir() {
			continue
		}
		candidate, ok, err := existingTargetTextSubtitleCandidate(folder, entry, languages)
		if err != nil {
			return subtitleCandidate{}, false, err
		}
		if !ok {
			continue
		}
		for id := range ids {
			if candidateMatchesLanguage(candidate, id) {
				candidates = append(candidates, candidate)
				break
			}
		}
	}
	if len(candidates) == 0 {
		return subtitleCandidate{}, false, nil
	}

	sort.Slice(candidates, func(i, j int) bool {
		iExt := supportedInputSubtitleExtensions[strings.ToLower(filepath.Ext(candidates[i].Name))]
		jExt := supportedInputSubtitleExtensions[strings.ToLower(filepath.Ext(candidates[j].Name))]
		if iExt != jExt {
			return iExt < jExt
		}
		if candidates[i].Size != candidates[j].Size {
			return candidates[i].Size > candidates[j].Size
		}
		return candidates[i].Name < candidates[j].Name
	})
	return candidates[0], true, nil
}

func existingTargetTextSubtitleCandidate(folder string, entry os.DirEntry, languages cueforge.Registry) (subtitleCandidate, bool, error) {
	name := entry.Name()
	ext := strings.ToLower(filepath.Ext(name))
	if _, ok := supportedInputSubtitleExtensions[ext]; !ok {
		return subtitleCandidate{}, false, nil
	}
	if ext == ".sup" {
		return subtitleCandidate{}, false, nil
	}

	langID := languageIDFromFilename(name)
	if strings.HasPrefix(strings.ToLower(name), "cueforge_") {
		langID = cueForgePlainOutputLanguageID(name)
	}
	if langID == "" {
		return subtitleCandidate{}, false, nil
	}

	path := filepath.Join(folder, name)
	candidate := subtitleCandidate{
		Path:       path,
		Name:       name,
		LanguageID: langID,
	}
	if ext == ".sub" {
		idxPath := matchingVobSubIndexPath(candidate.Path)
		if _, err := os.Stat(idxPath); err == nil {
			return subtitleCandidate{}, false, nil
		}
	}

	info, err := entry.Info()
	if err != nil {
		return subtitleCandidate{}, false, fmt.Errorf("stat subtitle candidate %s: %w", path, err)
	}
	candidate.Size = info.Size()
	if resolved, ok := languages.Resolve(langID); ok {
		candidate.Language = &resolved
	}
	return candidate, true, nil
}

func cueForgePlainOutputLanguageID(name string) string {
	stem := strings.TrimSuffix(filepath.Base(name), filepath.Ext(name))
	stem = strings.ToLower(stem)
	if !strings.HasPrefix(stem, "cueforge_") || strings.HasSuffix(stem, "_annotated") {
		return ""
	}
	id := strings.TrimPrefix(stem, "cueforge_")
	if !languageIDPattern.MatchString(id) {
		return ""
	}
	return id
}

func translateTarget(ctx context.Context, client *http.Client, cfg config.Config, locks *folderLocks, folder, mediaTitle string, input subtitleCandidate, target targetLanguage) error {
	return translateTargetWithSaveMode(ctx, client, cfg, locks, folder, mediaTitle, input, target, saveAllOutputs)
}

func translateTargetWithSaveMode(ctx context.Context, client *http.Client, cfg config.Config, locks *folderLocks, folder, mediaTitle string, input subtitleCandidate, target targetLanguage, saveMode outputSaveMode) error {
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
		if saveErr := saveFailedSubtitleOnError(cfg, locks, folder, mediaTitle, input, target, failedTranslateResponse{Error: err.Error()}); saveErr != nil {
			return fmt.Errorf("post translate request: %w; save failed subtitle: %v", err, saveErr)
		}
		return fmt.Errorf("post translate request: %w", err)
	}
	defer resp.Body.Close()

	body, err := io.ReadAll(resp.Body)
	if err != nil {
		errorResponse := failedTranslateResponse{
			StatusCode: resp.StatusCode,
			Status:     resp.Status,
			Error:      err.Error(),
		}
		if saveErr := saveFailedSubtitleOnError(cfg, locks, folder, mediaTitle, input, target, errorResponse); saveErr != nil {
			return fmt.Errorf("read translate response: %w; save failed subtitle: %v", err, saveErr)
		}
		return fmt.Errorf("read translate response: %w", err)
	}
	bodyText := strings.TrimSpace(string(body))
	if resp.StatusCode < http.StatusOK || resp.StatusCode >= http.StatusMultipleChoices {
		errorResponse := failedTranslateResponse{
			StatusCode: resp.StatusCode,
			Status:     resp.Status,
			Body:       bodyText,
		}
		if saveErr := saveFailedSubtitleOnError(cfg, locks, folder, mediaTitle, input, target, errorResponse); saveErr != nil {
			return fmt.Errorf("translate returned %s: %s; save failed subtitle: %w", resp.Status, errorResponse.Body, saveErr)
		}
		return fmt.Errorf("translate returned %s: %s", resp.Status, bodyText)
	}

	var decoded translateResponse
	if err := json.Unmarshal(body, &decoded); err != nil {
		errorResponse := failedTranslateResponse{
			StatusCode: resp.StatusCode,
			Status:     resp.Status,
			Body:       bodyText,
			Error:      err.Error(),
		}
		if saveErr := saveFailedSubtitleOnError(cfg, locks, folder, mediaTitle, input, target, errorResponse); saveErr != nil {
			return fmt.Errorf("decode translate JSON: %w; save failed subtitle: %v", err, saveErr)
		}
		return fmt.Errorf("decode translate JSON: %w", err)
	}
	if len(decoded.Outputs) == 0 {
		errorResponse := failedTranslateResponse{
			StatusCode: resp.StatusCode,
			Status:     resp.Status,
			Body:       bodyText,
			Error:      "translate response did not include outputs",
		}
		if saveErr := saveFailedSubtitleOnError(cfg, locks, folder, mediaTitle, input, target, errorResponse); saveErr != nil {
			return fmt.Errorf("translate response did not include outputs; save failed subtitle: %w", saveErr)
		}
		return errors.New("translate response did not include outputs")
	}

	if err := saveTargetOutputsWithMode(folder, input, target, decoded.Outputs, locks, saveMode); err != nil {
		return err
	}
	return nil
}

func translateRequestParametersFor(input subtitleCandidate, target targetLanguage, cfg config.Config, mediaTitle string) translateRequestParameters {
	params := translateRequestParameters{
		TargetLanguage: target.RequestValue,
		Response:       "json",
		OutputFormat:   []string{"ass", "vtt"},
		Annotate:       target.Annotate,
	}
	if input.Language != nil && input.LanguageID != "" {
		params.InputLanguage = input.LanguageID
	}
	if cfg.Model != "" {
		params.Model = cfg.Model
	}
	if cfg.VisionModel != "" {
		params.VisionModel = cfg.VisionModel
	}
	if cfg.ReasoningEffort != "" {
		params.ReasoningEffort = cfg.ReasoningEffort
	}
	if mediaTitle != "" {
		params.Media = mediaTitle
	}
	return params
}

func translateRequestFields(params translateRequestParameters) []translateRequestField {
	fields := []translateRequestField{
		{Name: "target_language", Value: params.TargetLanguage},
		{Name: "response", Value: params.Response},
	}
	for _, format := range params.OutputFormat {
		fields = append(fields, translateRequestField{Name: "output_format", Value: format})
	}
	if params.Annotate {
		fields = append(fields, translateRequestField{Name: "annotate", Value: "true"})
	}
	if params.InputLanguage != "" {
		fields = append(fields, translateRequestField{Name: "input_language", Value: params.InputLanguage})
	}
	if params.Model != "" {
		fields = append(fields, translateRequestField{Name: "model", Value: params.Model})
	}
	if params.VisionModel != "" {
		fields = append(fields, translateRequestField{Name: "vmodel", Value: params.VisionModel})
	}
	if params.ReasoningEffort != "" {
		fields = append(fields, translateRequestField{Name: "reasoning_effort", Value: params.ReasoningEffort})
	}
	if params.Media != "" {
		fields = append(fields, translateRequestField{Name: "media", Value: params.Media})
	}
	return fields
}

func saveFailedSubtitleOnError(cfg config.Config, locks *folderLocks, folder, mediaTitle string, input subtitleCandidate, target targetLanguage, errorResponse failedTranslateResponse) error {
	if !cfg.SaveOnError {
		return nil
	}

	sourceName := input.Name
	if sourceName == "" {
		sourceName = filepath.Base(input.Path)
	}
	idxName := input.IdxName
	if idxName == "" && input.IdxPath != "" {
		idxName = filepath.Base(input.IdxPath)
	}

	errorDir := strings.TrimSpace(cfg.ErrorDir)
	if errorDir == "" {
		errorDir = "./errors"
	}
	errorFolder := filepath.Join(errorDir, failedSubtitleFolderName(folder, mediaTitle))
	if err := os.MkdirAll(errorFolder, 0o755); err != nil {
		return fmt.Errorf("create error dir: %w", err)
	}
	if err := copyFileAtomic(input.Path, filepath.Join(errorFolder, sourceName), locks); err != nil {
		return fmt.Errorf("copy failed subtitle: %w", err)
	}
	if input.IdxPath != "" {
		if err := copyFileAtomic(input.IdxPath, filepath.Join(errorFolder, idxName), locks); err != nil {
			return fmt.Errorf("copy failed subtitle idx: %w", err)
		}
	}

	report := failedSubtitleReport{
		SourceFilename:    sourceName,
		SourcePath:        input.Path,
		IdxFilename:       idxName,
		IdxPath:           input.IdxPath,
		MediaTitle:        strings.TrimSpace(mediaTitle),
		OriginalFolder:    filepath.Base(folder),
		TargetLanguage:    target.RequestValue,
		Annotate:          target.Annotate,
		RequestParameters: translateRequestParametersFor(input, target, cfg, mediaTitle),
		ErrorResponse:     errorResponse,
	}
	data, err := json.MarshalIndent(report, "", "  ")
	if err != nil {
		return fmt.Errorf("encode failed subtitle report: %w", err)
	}
	if err := writeOutputFile(filepath.Join(errorFolder, sourceName+".json"), string(data)+"\n", locks, false); err != nil {
		return fmt.Errorf("write failed subtitle report: %w", err)
	}
	log.Printf("saved failed subtitle input=%s target=%s dir=%s", sourceName, target.OutputID, errorFolder)
	return nil
}

func failedSubtitleFolderName(folder, mediaTitle string) string {
	name := strings.TrimSpace(mediaTitle)
	if name == "" {
		name = filepath.Base(folder)
	}
	return safePathComponent(name)
}

func safePathComponent(value string) string {
	value = strings.TrimSpace(value)
	var b strings.Builder
	for _, r := range value {
		switch r {
		case '/', '\\':
			b.WriteByte('_')
		default:
			if r >= 0 && r < 32 {
				continue
			}
			b.WriteRune(r)
		}
	}
	value = strings.TrimSpace(b.String())
	if value == "" || value == "." || value == ".." {
		return "unknown"
	}
	return value
}

func buildTranslateRequest(input subtitleCandidate, target targetLanguage, cfg config.Config, locks *folderLocks, mediaTitle string) (io.Reader, string, error) {
	var body bytes.Buffer
	writer := multipart.NewWriter(&body)

	for _, field := range translateRequestFields(translateRequestParametersFor(input, target, cfg, mediaTitle)) {
		if err := writer.WriteField(field.Name, field.Value); err != nil {
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
	return saveTargetOutputsWithMode(folder, input, target, outputs, locks, saveAllOutputs)
}

func saveTargetOutputsWithMode(folder string, input subtitleCandidate, target targetLanguage, outputs []translateOutput, locks *folderLocks, saveMode outputSaveMode) error {
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
			if saveMode == saveAnnotatedOnlyOutputs {
				continue
			}
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
			if saveMode == saveAnnotatedOnlyOutputs {
				continue
			}
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
		if saveMode == saveAnnotatedOnlyOutputs {
			if !found["annotated:"+format] {
				return fmt.Errorf("translate response missing annotated %s output", format)
			}
			continue
		}
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

func copyFileAtomic(src, dst string, locks *folderLocks) (err error) {
	if samePath(src, dst) {
		return nil
	}
	if err := os.MkdirAll(filepath.Dir(dst), 0o755); err != nil {
		return fmt.Errorf("create destination dir: %w", err)
	}

	unlockSrc := lockFile(locks, src)
	defer unlockSrc()
	unlockDst := lockFile(locks, dst)
	defer unlockDst()

	in, err := os.Open(src)
	if err != nil {
		return fmt.Errorf("open source: %w", err)
	}
	defer in.Close()

	tmp, err := os.CreateTemp(filepath.Dir(dst), "."+filepath.Base(dst)+".tmp-*")
	if err != nil {
		return fmt.Errorf("create temp copy: %w", err)
	}
	tmpName := tmp.Name()
	removeTemp := true
	defer func() {
		if removeTemp {
			_ = os.Remove(tmpName)
		}
	}()

	if _, err := io.Copy(tmp, in); err != nil {
		_ = tmp.Close()
		return fmt.Errorf("write temp copy: %w", err)
	}
	if err := tmp.Chmod(0o644); err != nil {
		_ = tmp.Close()
		return fmt.Errorf("chmod temp copy: %w", err)
	}
	if err := tmp.Close(); err != nil {
		return fmt.Errorf("close temp copy: %w", err)
	}
	if err := os.Rename(tmpName, dst); err != nil {
		return fmt.Errorf("rename temp copy: %w", err)
	}
	removeTemp = false
	return nil
}

func samePath(a, b string) bool {
	absA, errA := filepath.Abs(a)
	absB, errB := filepath.Abs(b)
	if errA == nil && errB == nil {
		return absA == absB
	}
	return filepath.Clean(a) == filepath.Clean(b)
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

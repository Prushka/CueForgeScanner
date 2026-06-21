package scanner

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math/rand"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"CueForgeScanner/internal/config"
	"CueForgeScanner/internal/cueforge"
)

func TestChooseInputSubtitleUsesPriorityAliases(t *testing.T) {
	dir := t.TempDir()
	writeTestFile(t, filepath.Join(dir, "1-eng.ass"), "english")
	writeTestFile(t, filepath.Join(dir, "2-deu.ass"), "german")
	writeTestFile(t, filepath.Join(dir, "4-eng.sup"), "english image")
	writeTestFile(t, filepath.Join(dir, "cueforge_eng.ass"), "generated")
	writeTestFile(t, filepath.Join(dir, "3-fra.vtt"), "vtt")

	priorities := []inputLanguage{
		{Raw: "ger", IDs: map[string]struct{}{"ger": {}, "deu": {}}},
		{Raw: "eng", IDs: map[string]struct{}{"eng": {}}},
	}
	registry := mustRegistry(t)

	selected, err := chooseInputSubtitle(dir, priorities, registry, rand.New(rand.NewSource(1)))
	if err != nil {
		t.Fatalf("chooseInputSubtitle failed: %v", err)
	}
	if selected.Name != "2-deu.ass" {
		t.Fatalf("selected %q, want 2-deu.ass", selected.Name)
	}
}

func TestChooseInputSubtitleUsesFormatOrder(t *testing.T) {
	dir := t.TempDir()
	writeTestFile(t, filepath.Join(dir, "2-eng.ass"), "ass subtitle")
	writeTestFile(t, filepath.Join(dir, "4-eng.sup"), "image subtitle")
	writeTestFile(t, filepath.Join(dir, "3-eng.vtt"), "vtt subtitle")
	writeTestFile(t, filepath.Join(dir, "5-eng.sub"), "sub subtitle")

	priorities := []inputLanguage{
		{Raw: "eng", IDs: map[string]struct{}{"eng": {}}},
	}

	selected, err := chooseInputSubtitle(dir, priorities, mustRegistry(t), rand.New(rand.NewSource(1)))
	if err != nil {
		t.Fatalf("chooseInputSubtitle failed: %v", err)
	}
	if selected.Name != "2-eng.ass" {
		t.Fatalf("selected %q, want 2-eng.ass", selected.Name)
	}
}

func TestChooseInputSubtitleUsesLargestSameLanguageFormat(t *testing.T) {
	dir := t.TempDir()
	writeTestFile(t, filepath.Join(dir, "2-eng.ass"), "small")
	writeTestFile(t, filepath.Join(dir, "3-eng.ass"), strings.Repeat("large", 100))
	writeTestFile(t, filepath.Join(dir, "1-eng.vtt"), strings.Repeat("larger vtt", 100))

	priorities := []inputLanguage{
		{Raw: "eng", IDs: map[string]struct{}{"eng": {}}},
	}

	selected, err := chooseInputSubtitle(dir, priorities, mustRegistry(t), rand.New(rand.NewSource(1)))
	if err != nil {
		t.Fatalf("chooseInputSubtitle failed: %v", err)
	}
	if selected.Name != "3-eng.ass" {
		t.Fatalf("selected %q, want 3-eng.ass", selected.Name)
	}
}

func TestChooseInputSubtitleAttachesVobSubIndex(t *testing.T) {
	dir := t.TempDir()
	writeTestFile(t, filepath.Join(dir, "5-eng.sub"), "sub subtitle")
	writeTestFile(t, filepath.Join(dir, "5-eng.idx"), "idx subtitle")

	priorities := []inputLanguage{
		{Raw: "eng", IDs: map[string]struct{}{"eng": {}}},
	}

	selected, err := chooseInputSubtitle(dir, priorities, mustRegistry(t), rand.New(rand.NewSource(1)))
	if err != nil {
		t.Fatalf("chooseInputSubtitle failed: %v", err)
	}
	if selected.Name != "5-eng.sub" {
		t.Fatalf("selected %q, want 5-eng.sub", selected.Name)
	}
	if selected.IdxName != "5-eng.idx" || selected.IdxPath != filepath.Join(dir, "5-eng.idx") {
		t.Fatalf("selected idx = (%q, %q), want 5-eng.idx sidecar", selected.IdxName, selected.IdxPath)
	}
}

func TestListScanFoldersSortsByLatestModTimeFirst(t *testing.T) {
	dir := t.TempDir()
	older := filepath.Join(dir, "older")
	newer := filepath.Join(dir, "newer")
	tieA := filepath.Join(dir, "alpha")
	tieB := filepath.Join(dir, "beta")

	for _, path := range []string{older, newer, tieA, tieB} {
		if err := os.Mkdir(path, 0o755); err != nil {
			t.Fatalf("Mkdir(%s) failed: %v", path, err)
		}
	}

	base := time.Unix(1_700_000_000, 0)
	setDirModTime(t, older, base.Add(-2*time.Hour))
	setDirModTime(t, newer, base)
	setDirModTime(t, tieA, base.Add(-time.Hour))
	setDirModTime(t, tieB, base.Add(-time.Hour))

	folders, err := listScanFolders(dir)
	if err != nil {
		t.Fatalf("listScanFolders failed: %v", err)
	}

	got := make([]string, 0, len(folders))
	for _, folder := range folders {
		got = append(got, folder.Name)
	}

	want := []string{"newer", "alpha", "beta", "older"}
	if !slices.Equal(got, want) {
		t.Fatalf("listScanFolders = %#v, want %#v", got, want)
	}
}

func TestRunLimitsConcurrentFolderProcessing(t *testing.T) {
	dir := t.TempDir()
	for i := 0; i < 5; i++ {
		folder := filepath.Join(dir, fmt.Sprintf("folder-%d", i))
		if err := os.Mkdir(folder, 0o755); err != nil {
			t.Fatalf("Mkdir(%s) failed: %v", folder, err)
		}
		writeTestFile(t, filepath.Join(folder, "1-eng.ass"), "subtitle")
	}

	var currentRequests int64
	var maxRequests int64
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/translate" {
			http.NotFound(w, r)
			return
		}

		active := atomic.AddInt64(&currentRequests, 1)
		defer atomic.AddInt64(&currentRequests, -1)
		updateMaxInt64(&maxRequests, active)
		time.Sleep(50 * time.Millisecond)

		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(translateResponse{
			Outputs: []translateOutput{
				{Format: "ass", Variant: "translated", Content: "plain ass"},
				{Format: "vtt", Variant: "translated", Content: "plain vtt"},
			},
		})
	}))
	defer server.Close()

	cfg := config.Config{
		ScanDir:         dir,
		CueForgeBaseURL: server.URL,
		InputLanguages:  []string{"eng"},
		TargetLanguages: []string{"jpn"},
		Concurrency:     2,
	}

	if err := Run(context.Background(), server.Client(), rand.New(rand.NewSource(1)), cfg, mustRegistry(t)); err != nil {
		t.Fatalf("Run failed: %v", err)
	}
	if got := atomic.LoadInt64(&maxRequests); got != 2 {
		t.Fatalf("max concurrent translate requests = %d, want 2", got)
	}
}

func TestRunAppliesConcurrencyLimitToTargetLanguagesInSameFolder(t *testing.T) {
	dir := t.TempDir()
	folder := filepath.Join(dir, "episode")
	if err := os.Mkdir(folder, 0o755); err != nil {
		t.Fatalf("Mkdir(%s) failed: %v", folder, err)
	}
	writeTestFile(t, filepath.Join(folder, "1-eng.ass"), "subtitle")

	var currentRequests int64
	var maxRequests int64
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/translate" {
			http.NotFound(w, r)
			return
		}
		if err := r.ParseMultipartForm(1 << 20); err != nil {
			t.Fatalf("ParseMultipartForm failed: %v", err)
		}
		target := r.FormValue("target_language")

		active := atomic.AddInt64(&currentRequests, 1)
		defer atomic.AddInt64(&currentRequests, -1)
		updateMaxInt64(&maxRequests, active)
		time.Sleep(75 * time.Millisecond)

		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(translateResponse{
			Outputs: []translateOutput{
				{Format: "ass", Variant: "translated", Content: target + " ass"},
				{Format: "vtt", Variant: "translated", Content: target + " vtt"},
			},
		})
	}))
	defer server.Close()

	cfg := config.Config{
		ScanDir:         dir,
		CueForgeBaseURL: server.URL,
		InputLanguages:  []string{"eng"},
		TargetLanguages: []string{"jpn", "fre", "ger"},
		Concurrency:     2,
	}

	if err := Run(context.Background(), server.Client(), rand.New(rand.NewSource(1)), cfg, mustRegistry(t)); err != nil {
		t.Fatalf("Run failed: %v", err)
	}
	if got := atomic.LoadInt64(&maxRequests); got != 2 {
		t.Fatalf("max concurrent same-folder translate requests = %d, want 2", got)
	}
	assertFileContent(t, filepath.Join(folder, "cueforge_jpn.ass"), "jpn ass")
	assertFileContent(t, filepath.Join(folder, "cueforge_fre.ass"), "fre ass")
	assertFileContent(t, filepath.Join(folder, "cueforge_ger.ass"), "ger ass")
}

func TestRunAppliesConcurrencyLimitAcrossFoldersAndTargets(t *testing.T) {
	dir := t.TempDir()
	for i := 0; i < 3; i++ {
		folder := filepath.Join(dir, fmt.Sprintf("folder-%d", i))
		if err := os.Mkdir(folder, 0o755); err != nil {
			t.Fatalf("Mkdir(%s) failed: %v", folder, err)
		}
		writeTestFile(t, filepath.Join(folder, "1-eng.ass"), "subtitle")
	}

	var currentRequests int64
	var maxRequests int64
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/translate" {
			http.NotFound(w, r)
			return
		}
		if err := r.ParseMultipartForm(1 << 20); err != nil {
			t.Fatalf("ParseMultipartForm failed: %v", err)
		}
		target := r.FormValue("target_language")

		active := atomic.AddInt64(&currentRequests, 1)
		defer atomic.AddInt64(&currentRequests, -1)
		updateMaxInt64(&maxRequests, active)
		time.Sleep(75 * time.Millisecond)

		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(translateResponse{
			Outputs: []translateOutput{
				{Format: "ass", Variant: "translated", Content: target + " ass"},
				{Format: "vtt", Variant: "translated", Content: target + " vtt"},
			},
		})
	}))
	defer server.Close()

	cfg := config.Config{
		ScanDir:         dir,
		CueForgeBaseURL: server.URL,
		InputLanguages:  []string{"eng"},
		TargetLanguages: []string{"jpn", "fre", "ger"},
		Concurrency:     3,
	}

	if err := Run(context.Background(), server.Client(), rand.New(rand.NewSource(1)), cfg, mustRegistry(t)); err != nil {
		t.Fatalf("Run failed: %v", err)
	}
	if got := atomic.LoadInt64(&maxRequests); got != 3 {
		t.Fatalf("max concurrent translate requests = %d, want 3", got)
	}
}

func TestProcessFolderSerializesDuplicateTargetOutputs(t *testing.T) {
	dir := t.TempDir()
	writeTestFile(t, filepath.Join(dir, "1-eng.ass"), "subtitle")

	var requests int64
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/translate" {
			http.NotFound(w, r)
			return
		}
		atomic.AddInt64(&requests, 1)
		time.Sleep(50 * time.Millisecond)
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(translateResponse{
			Outputs: []translateOutput{
				{Format: "ass", Variant: "translated", Content: "french ass"},
				{Format: "vtt", Variant: "translated", Content: "french vtt"},
			},
		})
	}))
	defer server.Close()

	targets := []targetLanguage{
		{RequestValue: "fre", OutputID: "fre"},
		{RequestValue: "French", OutputID: "fre"},
	}
	errs := processFolder(context.Background(), server.Client(), newLockedRand(rand.New(rand.NewSource(1))), config.Config{CueForgeBaseURL: server.URL}, mustRegistry(t), []inputLanguage{{Raw: "eng", IDs: map[string]struct{}{"eng": {}}}}, targets, scanFolder{Name: "episode", Path: dir}, newTranslationLimiter(2))
	if len(errs) > 0 {
		t.Fatalf("processFolder errors = %#v, want none", errs)
	}
	if got := atomic.LoadInt64(&requests); got != 1 {
		t.Fatalf("requests = %d, want 1", got)
	}
	assertFileContent(t, filepath.Join(dir, "cueforge_fre.ass"), "french ass")
	assertFileContent(t, filepath.Join(dir, "cueforge_fre.vtt"), "french vtt")
}

func TestRunSkipsTargetWhenAllExpectedOutputsExist(t *testing.T) {
	dir := t.TempDir()
	writeTestFile(t, filepath.Join(dir, "1-eng.ass"), "subtitle")
	cutoff := time.Unix(1_700_000_000, 0)
	for _, name := range expectedOutputFileNames(subtitleCandidate{Name: "1-eng.ass", LanguageID: "eng"}, targetLanguage{OutputID: "jpn", Annotate: true}) {
		path := filepath.Join(dir, name)
		writeTestFile(t, path, "existing")
		setFileModTime(t, path, cutoff.Add(time.Second))
	}

	requests := int64(0)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt64(&requests, 1)
		t.Fatalf("unexpected request to %s", r.URL.Path)
	}))
	defer server.Close()

	errs := processFolder(context.Background(), server.Client(), newLockedRand(rand.New(rand.NewSource(1))), config.Config{CueForgeBaseURL: server.URL, SkipGeneratedAfter: cutoff}, mustRegistry(t), []inputLanguage{{Raw: "eng", IDs: map[string]struct{}{"eng": {}}}}, []targetLanguage{{RequestValue: "jpn", OutputID: "jpn", Annotate: true}}, scanFolder{Name: "episode", Path: dir}, newTranslationLimiter(1))
	if len(errs) > 0 {
		t.Fatalf("processFolder errors = %#v, want none", errs)
	}
	if got := atomic.LoadInt64(&requests); got != 0 {
		t.Fatalf("requests = %d, want 0", got)
	}
}

func TestRunSkipsUnannotatedTextTargetWhenPlainTargetExistsEvenIfOlderThanCutoff(t *testing.T) {
	dir := t.TempDir()
	writeTestFile(t, filepath.Join(dir, "1-eng.ass"), "subtitle")
	cutoff := time.Unix(1_700_000_000, 0)
	for _, name := range expectedOutputFileNames(subtitleCandidate{Name: "1-eng.ass", LanguageID: "eng"}, targetLanguage{OutputID: "jpn"}) {
		path := filepath.Join(dir, name)
		writeTestFile(t, path, "existing")
		setFileModTime(t, path, cutoff.Add(-time.Second))
	}

	requests := int64(0)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt64(&requests, 1)
		t.Fatalf("unexpected request to %s", r.URL.Path)
	}))
	defer server.Close()

	errs := processFolder(context.Background(), server.Client(), newLockedRand(rand.New(rand.NewSource(1))), config.Config{CueForgeBaseURL: server.URL, SkipGeneratedAfter: cutoff}, mustRegistry(t), []inputLanguage{{Raw: "eng", IDs: map[string]struct{}{"eng": {}}}}, []targetLanguage{{RequestValue: "jpn", OutputID: "jpn"}}, scanFolder{Name: "episode", Path: dir}, newTranslationLimiter(1))
	if len(errs) > 0 {
		t.Fatalf("processFolder errors = %#v, want none", errs)
	}
	if got := atomic.LoadInt64(&requests); got != 0 {
		t.Fatalf("requests = %d, want 0", got)
	}
	assertFileContent(t, filepath.Join(dir, "cueforge_jpn.ass"), "existing")
	assertFileContent(t, filepath.Join(dir, "cueforge_jpn.vtt"), "existing")
}

func TestRunSkipsUnannotatedTextTargetWhenAnyPlainTargetFormatExists(t *testing.T) {
	dir := t.TempDir()
	writeTestFile(t, filepath.Join(dir, "1-eng.ass"), "subtitle")
	writeTestFile(t, filepath.Join(dir, "cueforge_jpn.ass"), "existing")

	requests := int64(0)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt64(&requests, 1)
		t.Fatalf("unexpected request to %s", r.URL.Path)
	}))
	defer server.Close()

	errs := processFolder(context.Background(), server.Client(), newLockedRand(rand.New(rand.NewSource(1))), config.Config{CueForgeBaseURL: server.URL}, mustRegistry(t), []inputLanguage{{Raw: "eng", IDs: map[string]struct{}{"eng": {}}}}, []targetLanguage{{RequestValue: "jpn", OutputID: "jpn"}}, scanFolder{Name: "episode", Path: dir}, newTranslationLimiter(1))
	if len(errs) > 0 {
		t.Fatalf("processFolder errors = %#v, want none", errs)
	}
	if got := atomic.LoadInt64(&requests); got != 0 {
		t.Fatalf("requests = %d, want 0", got)
	}
	assertFileContent(t, filepath.Join(dir, "cueforge_jpn.ass"), "existing")
	assertFileNotExists(t, filepath.Join(dir, "cueforge_jpn.vtt"))
}

func TestRunAnnotatesExistingTextTargetAndSavesOnlyAnnotatedOutputs(t *testing.T) {
	dir := t.TempDir()
	writeTestFile(t, filepath.Join(dir, "1-eng.ass"), "english")
	writeTestFile(t, filepath.Join(dir, "cueforge_chi.ass"), "existing chinese")

	requests := int64(0)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt64(&requests, 1)
		if err := r.ParseMultipartForm(1 << 20); err != nil {
			t.Fatalf("ParseMultipartForm failed: %v", err)
		}

		assertFormValue(t, r, "target_language", "chi")
		assertFormValue(t, r, "annotate", "true")
		assertFormValue(t, r, "input_language", "chi")
		assertFormValues(t, r, "output_format", []string{"ass", "vtt"})

		file, header, err := r.FormFile("file")
		if err != nil {
			t.Fatalf("FormFile failed: %v", err)
		}
		defer file.Close()
		if header.Filename != "cueforge_chi.ass" {
			t.Fatalf("uploaded filename = %q, want cueforge_chi.ass", header.Filename)
		}
		body, err := io.ReadAll(file)
		if err != nil {
			t.Fatalf("ReadAll upload failed: %v", err)
		}
		if string(body) != "existing chinese" {
			t.Fatalf("uploaded body = %q, want existing chinese", body)
		}

		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(translateResponse{
			Outputs: []translateOutput{
				{Format: "ass", Variant: "translated", Content: "plain ass"},
				{Format: "vtt", Variant: "translated", Content: "plain vtt"},
				{Format: "ass", Variant: "annotated", Annotated: true, Content: "annotated ass"},
				{Format: "vtt", Variant: "annotated", Annotated: true, Content: "annotated vtt"},
			},
		})
	}))
	defer server.Close()

	errs := processFolder(context.Background(), server.Client(), newLockedRand(rand.New(rand.NewSource(1))), config.Config{CueForgeBaseURL: server.URL}, mustRegistry(t), []inputLanguage{{Raw: "eng", IDs: map[string]struct{}{"eng": {}}}}, []targetLanguage{{RequestValue: "chi", OutputID: "chi", Annotate: true}}, scanFolder{Name: "episode", Path: dir}, newTranslationLimiter(1))
	if len(errs) > 0 {
		t.Fatalf("processFolder errors = %#v, want none", errs)
	}
	if got := atomic.LoadInt64(&requests); got != 1 {
		t.Fatalf("requests = %d, want 1", got)
	}
	assertFileContent(t, filepath.Join(dir, "cueforge_chi.ass"), "existing chinese")
	assertFileNotExists(t, filepath.Join(dir, "cueforge_chi.vtt"))
	assertFileContent(t, filepath.Join(dir, "cueforge_chi_annotated.ass"), "annotated ass")
	assertFileContent(t, filepath.Join(dir, "cueforge_chi_annotated.vtt"), "annotated vtt")
}

func TestRunDoesNotUseExistingTextTargetShortcutForOCRInput(t *testing.T) {
	dir := t.TempDir()
	writeTestFile(t, filepath.Join(dir, "4-eng.sup"), "image subtitle")
	writeTestFile(t, filepath.Join(dir, "cueforge_jpn.ass"), "existing")

	requests := int64(0)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt64(&requests, 1)
		if err := r.ParseMultipartForm(1 << 20); err != nil {
			t.Fatalf("ParseMultipartForm failed: %v", err)
		}
		file, header, err := r.FormFile("file")
		if err != nil {
			t.Fatalf("FormFile failed: %v", err)
		}
		defer file.Close()
		if header.Filename != "4-eng.sup" {
			t.Fatalf("uploaded filename = %q, want 4-eng.sup", header.Filename)
		}

		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(translateResponse{
			Outputs: []translateOutput{
				{Format: "ass", Variant: "translated", Content: "translated ass"},
				{Format: "vtt", Variant: "translated", Content: "translated vtt"},
				{Format: "ass", Variant: "ocr_original", OCROriginal: true, Content: "ocr ass"},
				{Format: "vtt", Variant: "ocr_original", OCROriginal: true, Content: "ocr vtt"},
			},
		})
	}))
	defer server.Close()

	errs := processFolder(context.Background(), server.Client(), newLockedRand(rand.New(rand.NewSource(1))), config.Config{CueForgeBaseURL: server.URL}, mustRegistry(t), []inputLanguage{{Raw: "eng", IDs: map[string]struct{}{"eng": {}}}}, []targetLanguage{{RequestValue: "jpn", OutputID: "jpn"}}, scanFolder{Name: "episode", Path: dir}, newTranslationLimiter(1))
	if len(errs) > 0 {
		t.Fatalf("processFolder errors = %#v, want none", errs)
	}
	if got := atomic.LoadInt64(&requests); got != 1 {
		t.Fatalf("requests = %d, want 1", got)
	}
	assertFileContent(t, filepath.Join(dir, "cueforge_jpn.ass"), "translated ass")
	assertFileContent(t, filepath.Join(dir, "cueforge_jpn.vtt"), "translated vtt")
	assertFileContent(t, filepath.Join(dir, "cueforge_eng.ass"), "ocr ass")
}

func TestTranslateTargetPostsCueForgeFieldsAndSavesOutputs(t *testing.T) {
	dir := t.TempDir()
	inputPath := filepath.Join(dir, "6-eng.ass")
	writeTestFile(t, inputPath, "subtitle")

	requestSeen := false
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requestSeen = true
		if r.URL.Path != "/translate" {
			t.Fatalf("path = %s, want /translate", r.URL.Path)
		}
		if err := r.ParseMultipartForm(1 << 20); err != nil {
			t.Fatalf("ParseMultipartForm failed: %v", err)
		}

		assertFormValue(t, r, "target_language", "jpn")
		assertFormValue(t, r, "response", "json")
		assertFormValue(t, r, "annotate", "true")
		assertFormValue(t, r, "input_language", "eng")
		assertFormValue(t, r, "model", "gpt-test")
		assertFormValue(t, r, "reasoning_effort", "medium")
		assertFormValue(t, r, "media", "Episode title")
		assertFormValues(t, r, "output_format", []string{"ass", "vtt"})

		file, header, err := r.FormFile("file")
		if err != nil {
			t.Fatalf("FormFile failed: %v", err)
		}
		defer file.Close()
		if header.Filename != "6-eng.ass" {
			t.Fatalf("uploaded filename = %q, want 6-eng.ass", header.Filename)
		}
		body, err := io.ReadAll(file)
		if err != nil {
			t.Fatalf("ReadAll upload failed: %v", err)
		}
		if string(body) != "subtitle" {
			t.Fatalf("uploaded body = %q, want subtitle", body)
		}

		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(translateResponse{
			Outputs: []translateOutput{
				{Format: "ass", Variant: "translated", Content: "plain ass"},
				{Format: "ass", Variant: "annotated", Annotated: true, Content: "annotated ass"},
				{Format: "vtt", Variant: "translated", Content: "plain vtt"},
				{Format: "vtt", Variant: "annotated", Annotated: true, Content: "annotated vtt"},
			},
		})
	}))
	defer server.Close()

	registry := mustRegistry(t)
	lang, ok := registry.Resolve("eng")
	if !ok {
		t.Fatal("Resolve(eng) failed")
	}
	input := subtitleCandidate{
		Path:       inputPath,
		Name:       "6-eng.ass",
		LanguageID: "eng",
		Language:   &lang,
	}
	cfg := config.Config{
		CueForgeBaseURL: server.URL,
		Model:           "gpt-test",
		ReasoningEffort: "medium",
	}
	target := targetLanguage{
		RequestValue: "jpn",
		OutputID:     "jpn",
		Annotate:     true,
	}

	if err := translateTarget(context.Background(), server.Client(), cfg, newFolderLocks(), dir, "Episode title", input, target); err != nil {
		t.Fatalf("translateTarget failed: %v", err)
	}
	if !requestSeen {
		t.Fatal("server did not receive request")
	}

	assertFileContent(t, filepath.Join(dir, "cueforge_jpn.ass"), "plain ass")
	assertFileContent(t, filepath.Join(dir, "cueforge_jpn_annotated.ass"), "annotated ass")
	assertFileContent(t, filepath.Join(dir, "cueforge_jpn.vtt"), "plain vtt")
	assertFileContent(t, filepath.Join(dir, "cueforge_jpn_annotated.vtt"), "annotated vtt")
}

func TestTranslateTargetSavesFailedSubtitleAndMetadataOnError(t *testing.T) {
	dir := t.TempDir()
	errorDir := t.TempDir()
	inputPath := filepath.Join(dir, "2-eng.ass")
	writeTestFile(t, inputPath, "subtitle")

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if err := r.ParseMultipartForm(1 << 20); err != nil {
			t.Fatalf("ParseMultipartForm failed: %v", err)
		}
		assertFormValue(t, r, "target_language", "jpn")
		assertFormValue(t, r, "annotate", "true")
		assertFormValue(t, r, "input_language", "eng")
		http.Error(w, `{"error":"translation failed"}`, http.StatusInternalServerError)
	}))
	defer server.Close()

	registry := mustRegistry(t)
	lang, ok := registry.Resolve("eng")
	if !ok {
		t.Fatal("Resolve(eng) failed")
	}
	input := subtitleCandidate{
		Path:       inputPath,
		Name:       "2-eng.ass",
		LanguageID: "eng",
		Language:   &lang,
	}
	cfg := config.Config{
		CueForgeBaseURL: server.URL,
		Model:           "gpt-test",
		ReasoningEffort: "medium",
		SaveOnError:     true,
		ErrorDir:        errorDir,
	}
	target := targetLanguage{RequestValue: "jpn", OutputID: "jpn", Annotate: true}

	err := translateTarget(context.Background(), server.Client(), cfg, newFolderLocks(), dir, "Episode/Title", input, target)
	if err == nil {
		t.Fatal("translateTarget succeeded, want error")
	}
	if !strings.Contains(err.Error(), "translate returned 500 Internal Server Error") {
		t.Fatalf("translateTarget error = %v, want 500 status", err)
	}

	failedFolder := filepath.Join(errorDir, "Episode_Title")
	assertFileContent(t, filepath.Join(failedFolder, "2-eng.ass"), "subtitle")

	reportPath := filepath.Join(failedFolder, "2-eng.ass.json")
	data, err := os.ReadFile(reportPath)
	if err != nil {
		t.Fatalf("ReadFile(%s) failed: %v", reportPath, err)
	}
	var report failedSubtitleReport
	if err := json.Unmarshal(data, &report); err != nil {
		t.Fatalf("Unmarshal failed report failed: %v", err)
	}
	if report.SourceFilename != "2-eng.ass" || report.TargetLanguage != "jpn" || !report.Annotate {
		t.Fatalf("report basic fields = %#v, want source, target, annotation", report)
	}
	if report.MediaTitle != "Episode/Title" || report.OriginalFolder != filepath.Base(dir) {
		t.Fatalf("report folder fields = %#v, want media title and original folder", report)
	}
	if report.RequestParameters.TargetLanguage != "jpn" || !report.RequestParameters.Annotate || report.RequestParameters.InputLanguage != "eng" {
		t.Fatalf("request parameters = %#v, want target, annotation, and input language", report.RequestParameters)
	}
	if !slices.Equal(report.RequestParameters.OutputFormat, []string{"ass", "vtt"}) {
		t.Fatalf("output formats = %#v, want ass/vtt", report.RequestParameters.OutputFormat)
	}
	if report.RequestParameters.Model != "gpt-test" || report.RequestParameters.ReasoningEffort != "medium" || report.RequestParameters.Media != "Episode/Title" {
		t.Fatalf("optional request parameters = %#v, want model/reasoning/media", report.RequestParameters)
	}
	if report.ErrorResponse.StatusCode != http.StatusInternalServerError || !strings.Contains(report.ErrorResponse.Body, "translation failed") {
		t.Fatalf("error response = %#v, want 500 body", report.ErrorResponse)
	}
}

func TestTranslateTargetSavesFailedSubtitleUnderOriginalFolderWhenMediaTitleMissing(t *testing.T) {
	dir := t.TempDir()
	errorDir := t.TempDir()
	inputPath := filepath.Join(dir, "2-eng.ass")
	writeTestFile(t, inputPath, "subtitle")

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, "translation failed", http.StatusBadGateway)
	}))
	defer server.Close()

	cfg := config.Config{CueForgeBaseURL: server.URL, SaveOnError: true, ErrorDir: errorDir}
	target := targetLanguage{RequestValue: "jpn", OutputID: "jpn"}
	input := subtitleCandidate{Path: inputPath, Name: "2-eng.ass", LanguageID: "eng"}

	if err := translateTarget(context.Background(), server.Client(), cfg, newFolderLocks(), dir, "", input, target); err == nil {
		t.Fatal("translateTarget succeeded, want error")
	}

	failedFolder := filepath.Join(errorDir, filepath.Base(dir))
	assertFileContent(t, filepath.Join(failedFolder, "2-eng.ass"), "subtitle")
	if _, err := os.Stat(filepath.Join(failedFolder, "2-eng.ass.json")); err != nil {
		t.Fatalf("Stat failed report failed: %v", err)
	}
}

func TestTranslateTargetSavesOCROriginalOutputsForImageInput(t *testing.T) {
	dir := t.TempDir()
	inputPath := filepath.Join(dir, "4-eng.sup")
	writeTestFile(t, inputPath, "image subtitle")

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if err := r.ParseMultipartForm(1 << 20); err != nil {
			t.Fatalf("ParseMultipartForm failed: %v", err)
		}
		file, header, err := r.FormFile("file")
		if err != nil {
			t.Fatalf("FormFile failed: %v", err)
		}
		defer file.Close()
		if header.Filename != "4-eng.sup" {
			t.Fatalf("uploaded filename = %q, want 4-eng.sup", header.Filename)
		}

		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(translateResponse{
			Outputs: []translateOutput{
				{Format: "ass", Variant: "translated", Content: "translated ass"},
				{Format: "vtt", Variant: "translated", Content: "translated vtt"},
				{Format: "ass", Variant: "ocr_original", OCROriginal: true, Content: "ocr ass"},
				{Format: "vtt", Variant: "ocr_original", OCROriginal: true, Content: "ocr vtt"},
			},
		})
	}))
	defer server.Close()

	registry := mustRegistry(t)
	lang, ok := registry.Resolve("eng")
	if !ok {
		t.Fatal("Resolve(eng) failed")
	}
	input := subtitleCandidate{
		Path:       inputPath,
		Name:       "4-eng.sup",
		LanguageID: "eng",
		Language:   &lang,
	}
	cfg := config.Config{CueForgeBaseURL: server.URL}
	target := targetLanguage{RequestValue: "jpn", OutputID: "jpn"}

	if err := translateTarget(context.Background(), server.Client(), cfg, newFolderLocks(), dir, "", input, target); err != nil {
		t.Fatalf("translateTarget failed: %v", err)
	}

	assertFileContent(t, filepath.Join(dir, "cueforge_jpn.ass"), "translated ass")
	assertFileContent(t, filepath.Join(dir, "cueforge_jpn.vtt"), "translated vtt")
	assertFileContent(t, filepath.Join(dir, "cueforge_eng.ass"), "ocr ass")
	assertFileContent(t, filepath.Join(dir, "cueforge_eng.vtt"), "ocr vtt")
}

func TestSaveTargetOutputsWritesSharedOCROriginalOncePerRun(t *testing.T) {
	dir := t.TempDir()
	locks := newFolderLocks()
	input := subtitleCandidate{Name: "4-eng.sup", LanguageID: "eng"}

	var done int64
	start := make(chan struct{})
	errs := make(chan error, 2)
	for _, tt := range []struct {
		target targetLanguage
		ocr    string
	}{
		{target: targetLanguage{OutputID: "jpn"}, ocr: "ocr from jpn"},
		{target: targetLanguage{OutputID: "fre"}, ocr: "ocr from fre"},
	} {
		go func(target targetLanguage, ocr string) {
			<-start
			errs <- saveTargetOutputs(dir, input, target, []translateOutput{
				{Format: "ass", Variant: "translated", Content: target.OutputID + " ass"},
				{Format: "vtt", Variant: "translated", Content: target.OutputID + " vtt"},
				{Format: "ass", Variant: "ocr_original", OCROriginal: true, Content: ocr},
				{Format: "vtt", Variant: "ocr_original", OCROriginal: true, Content: ocr},
			}, locks)
			atomic.AddInt64(&done, 1)
		}(tt.target, tt.ocr)
	}
	close(start)

	for i := 0; i < 2; i++ {
		if err := <-errs; err != nil {
			t.Fatalf("saveTargetOutputs failed: %v", err)
		}
	}
	if got := atomic.LoadInt64(&done); got != 2 {
		t.Fatalf("completed saves = %d, want 2", got)
	}

	assertFileContent(t, filepath.Join(dir, "cueforge_jpn.ass"), "jpn ass")
	assertFileContent(t, filepath.Join(dir, "cueforge_fre.ass"), "fre ass")
	got, err := os.ReadFile(filepath.Join(dir, "cueforge_eng.ass"))
	if err != nil {
		t.Fatalf("ReadFile OCR output failed: %v", err)
	}
	if string(got) != "ocr from jpn" && string(got) != "ocr from fre" {
		t.Fatalf("OCR output = %q, want one complete writer's content", got)
	}
}

func TestTranslateTargetSendsVobSubIndexAndSavesOCROriginalOutputs(t *testing.T) {
	dir := t.TempDir()
	inputPath := filepath.Join(dir, "5-eng.sub")
	idxPath := filepath.Join(dir, "5-eng.idx")
	writeTestFile(t, inputPath, "binary subtitle")
	writeTestFile(t, idxPath, "IDX")

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if err := r.ParseMultipartForm(1 << 20); err != nil {
			t.Fatalf("ParseMultipartForm failed: %v", err)
		}
		if _, _, err := r.FormFile("idx"); err != nil {
			t.Fatalf("expected idx multipart file: %v", err)
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(translateResponse{
			Outputs: []translateOutput{
				{Format: "ass", Variant: "translated", Content: "translated ass"},
				{Format: "vtt", Variant: "translated", Content: "translated vtt"},
				{Format: "ass", Variant: "ocr_original", OCROriginal: true, Content: "ocr ass"},
				{Format: "vtt", Variant: "ocr_original", OCROriginal: true, Content: "ocr vtt"},
			},
		})
	}))
	defer server.Close()

	registry := mustRegistry(t)
	lang, ok := registry.Resolve("eng")
	if !ok {
		t.Fatal("Resolve(eng) failed")
	}
	input := subtitleCandidate{
		Path:       inputPath,
		Name:       "5-eng.sub",
		LanguageID: "eng",
		Language:   &lang,
		IdxPath:    idxPath,
		IdxName:    "5-eng.idx",
	}
	cfg := config.Config{CueForgeBaseURL: server.URL}
	target := targetLanguage{RequestValue: "jpn", OutputID: "jpn"}

	if err := translateTarget(context.Background(), server.Client(), cfg, newFolderLocks(), dir, "", input, target); err != nil {
		t.Fatalf("translateTarget failed: %v", err)
	}

	assertFileContent(t, filepath.Join(dir, "cueforge_jpn.ass"), "translated ass")
	assertFileContent(t, filepath.Join(dir, "cueforge_jpn.vtt"), "translated vtt")
	assertFileContent(t, filepath.Join(dir, "cueforge_eng.ass"), "ocr ass")
	assertFileContent(t, filepath.Join(dir, "cueforge_eng.vtt"), "ocr vtt")
}

func TestMediaTitleFromJob(t *testing.T) {
	tests := []struct {
		name    string
		jobJSON string
		want    string
	}{
		{
			name:    "title",
			jobJSON: `{"media":{"title":"  Teachings of the Witch WEBRip-2160p  "}}`,
			want:    "Teachings of the Witch WEBRip-2160p",
		},
		{
			name:    "null media",
			jobJSON: `{"media":null}`,
		},
		{
			name:    "blank title",
			jobJSON: `{"media":{"title":"   "}}`,
		},
		{
			name:    "invalid json",
			jobJSON: `{`,
		},
		{
			name: "missing job",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			dir := t.TempDir()
			if tt.jobJSON != "" {
				writeTestFile(t, filepath.Join(dir, "job.json"), tt.jobJSON)
			}

			if got := mediaTitleFromJob(dir, newFolderLocks()); got != tt.want {
				t.Fatalf("mediaTitleFromJob = %q, want %q", got, tt.want)
			}
		})
	}
}

func TestTargetLogFields(t *testing.T) {
	tests := []struct {
		name   string
		target targetLanguage
		want   string
	}{
		{
			name:   "plain target",
			target: targetLanguage{RequestValue: "jpn", OutputID: "jpn"},
			want:   "target=jpn annotation=off",
		},
		{
			name:   "annotated target",
			target: targetLanguage{RequestValue: "jpn", OutputID: "jpn", Annotate: true},
			want:   "target=jpn annotation=on",
		},
		{
			name:   "alias target",
			target: targetLanguage{RequestValue: "French", OutputID: "fre"},
			want:   `target=fre request_target="French" annotation=off`,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := targetLogFields(tt.target); got != tt.want {
				t.Fatalf("targetLogFields = %q, want %q", got, tt.want)
			}
		})
	}
}

func TestExpectedOutputFileNames(t *testing.T) {
	tests := []struct {
		name   string
		input  subtitleCandidate
		target targetLanguage
		want   []string
	}{
		{
			name:   "sup includes OCR originals",
			input:  subtitleCandidate{Name: "4-eng.sup", LanguageID: "eng"},
			target: targetLanguage{OutputID: "jpn", Annotate: true},
			want: []string{
				"cueforge_jpn.ass",
				"cueforge_jpn.vtt",
				"cueforge_jpn_annotated.ass",
				"cueforge_jpn_annotated.vtt",
				"cueforge_eng.ass",
				"cueforge_eng.vtt",
			},
		},
		{
			name:   "text sub excludes OCR originals",
			input:  subtitleCandidate{Name: "5-eng.sub", LanguageID: "eng"},
			target: targetLanguage{OutputID: "jpn"},
			want: []string{
				"cueforge_jpn.ass",
				"cueforge_jpn.vtt",
			},
		},
		{
			name:   "vobsub includes OCR originals",
			input:  subtitleCandidate{Name: "5-eng.sub", LanguageID: "eng", IdxPath: "/tmp/5-eng.idx"},
			target: targetLanguage{OutputID: "jpn"},
			want: []string{
				"cueforge_jpn.ass",
				"cueforge_jpn.vtt",
				"cueforge_eng.ass",
				"cueforge_eng.vtt",
			},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := expectedOutputFileNames(tt.input, tt.target)
			if !slices.Equal(got, tt.want) {
				t.Fatalf("expectedOutputFileNames = %#v, want %#v", got, tt.want)
			}
		})
	}
}

func writeTestFile(t *testing.T, path, content string) {
	t.Helper()
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatalf("WriteFile(%s) failed: %v", path, err)
	}
}

func setDirModTime(t *testing.T, path string, modTime time.Time) {
	t.Helper()
	if err := os.Chtimes(path, modTime, modTime); err != nil {
		t.Fatalf("Chtimes(%s) failed: %v", path, err)
	}
}

func setFileModTime(t *testing.T, path string, modTime time.Time) {
	t.Helper()
	if err := os.Chtimes(path, modTime, modTime); err != nil {
		t.Fatalf("Chtimes(%s) failed: %v", path, err)
	}
}

func updateMaxInt64(target *int64, value int64) {
	for {
		current := atomic.LoadInt64(target)
		if value <= current || atomic.CompareAndSwapInt64(target, current, value) {
			return
		}
	}
}

func assertFormValue(t *testing.T, r *http.Request, key, want string) {
	t.Helper()
	if got := r.FormValue(key); got != want {
		t.Fatalf("%s = %q, want %q", key, got, want)
	}
}

func assertFormValues(t *testing.T, r *http.Request, key string, want []string) {
	t.Helper()
	got := r.MultipartForm.Value[key]
	if len(got) != len(want) {
		t.Fatalf("%s = %#v, want %#v", key, got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("%s = %#v, want %#v", key, got, want)
		}
	}
}

func assertFileContent(t *testing.T, path, want string) {
	t.Helper()
	got, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("ReadFile(%s) failed: %v", path, err)
	}
	if !bytes.Equal(got, []byte(want)) {
		t.Fatalf("%s = %q, want %q", path, got, want)
	}
}

func assertFileNotExists(t *testing.T, path string) {
	t.Helper()
	if _, err := os.Stat(path); err == nil {
		t.Fatalf("%s exists, want missing", path)
	} else if !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("Stat(%s) failed: %v", path, err)
	}
}

func TestResolveTargetLanguagesPreservesConfiguredIDAndAnnotation(t *testing.T) {
	targets, err := resolveTargetLanguages([]string{"chi", "$fra", "French"}, mustRegistry(t))
	if err != nil {
		t.Fatalf("resolveTargetLanguages failed: %v", err)
	}
	if len(targets) != 3 {
		t.Fatalf("len(targets) = %d, want 3", len(targets))
	}
	if targets[0].OutputID != "chi" || targets[0].Annotate {
		t.Fatalf("target 0 = %#v, want chi without annotation", targets[0])
	}
	if targets[1].OutputID != "fra" || !targets[1].Annotate {
		t.Fatalf("target 1 = %#v, want fra with annotation", targets[1])
	}
	if targets[2].OutputID != "fre" || targets[2].Annotate {
		t.Fatalf("target 2 = %#v, want canonical fre without annotation", targets[2])
	}
}

func mustRegistry(t *testing.T) cueforge.Registry {
	t.Helper()
	registry, err := cueforge.NewRegistry([]cueforge.Language{
		{IDs: []string{"chi", "zho"}, LLMName: "SIMPLIFIED Chinese"},
		{IDs: []string{"eng"}, LLMName: "English"},
		{IDs: []string{"fre", "fra"}, LLMName: "French"},
		{IDs: []string{"ger", "deu"}, LLMName: "German"},
		{IDs: []string{"jpn"}, LLMName: "Japanese"},
	})
	if err != nil {
		t.Fatalf("NewRegistry failed: %v", err)
	}
	return registry
}

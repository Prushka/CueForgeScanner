package scanner

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"math/rand"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"slices"
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

	if err := translateTarget(context.Background(), server.Client(), cfg, dir, "Episode title", input, target); err != nil {
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

			if got := mediaTitleFromJob(dir); got != tt.want {
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

func TestOutputFileNames(t *testing.T) {
	got := outputFileNames(targetLanguage{OutputID: "jpn", Annotate: true})
	want := []string{
		"cueforge_jpn.ass",
		"cueforge_jpn.vtt",
		"cueforge_jpn_annotated.ass",
		"cueforge_jpn_annotated.vtt",
	}
	if !slices.Equal(got, want) {
		t.Fatalf("outputFileNames = %#v, want %#v", got, want)
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

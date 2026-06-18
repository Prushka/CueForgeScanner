package cueforge

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
)

type Language struct {
	IDs     []string `json:"ids"`
	LLMName string   `json:"llm_name"`
}

type LanguagesResponse struct {
	Languages []Language `json:"languages"`
}

type Registry struct {
	languages []Language
}

func FetchLanguages(ctx context.Context, client *http.Client, baseURL string) (Registry, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, strings.TrimRight(baseURL, "/")+"/languages", nil)
	if err != nil {
		return Registry{}, fmt.Errorf("create languages request: %w", err)
	}

	resp, err := client.Do(req)
	if err != nil {
		return Registry{}, fmt.Errorf("get CueForge languages: %w", err)
	}
	defer resp.Body.Close()

	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return Registry{}, fmt.Errorf("read CueForge languages response: %w", err)
	}
	if resp.StatusCode < http.StatusOK || resp.StatusCode >= http.StatusMultipleChoices {
		return Registry{}, fmt.Errorf("CueForge languages returned %s: %s", resp.Status, strings.TrimSpace(string(body)))
	}

	var decoded LanguagesResponse
	if err := json.Unmarshal(body, &decoded); err != nil {
		return Registry{}, fmt.Errorf("decode CueForge languages response: %w", err)
	}
	return NewRegistry(decoded.Languages)
}

func NewRegistry(languages []Language) (Registry, error) {
	if len(languages) == 0 {
		return Registry{}, errors.New("CueForge languages response did not include languages")
	}
	out := make([]Language, 0, len(languages))
	for _, lang := range languages {
		if len(lang.IDs) == 0 {
			return Registry{}, fmt.Errorf("CueForge language %q has no ids", lang.LLMName)
		}
		ids := make([]string, 0, len(lang.IDs))
		for _, id := range lang.IDs {
			id = strings.ToLower(strings.TrimSpace(id))
			if id == "" {
				return Registry{}, fmt.Errorf("CueForge language %q has an empty id", lang.LLMName)
			}
			ids = append(ids, id)
		}
		out = append(out, Language{
			IDs:     ids,
			LLMName: strings.TrimSpace(lang.LLMName),
		})
	}
	return Registry{languages: out}, nil
}

func (r Registry) Resolve(input string) (Language, bool) {
	needle := normalize(input)
	if needle == "" {
		return Language{}, false
	}
	for _, lang := range r.languages {
		if normalize(lang.LLMName) == needle {
			return lang, true
		}
		for _, id := range lang.IDs {
			if normalize(id) == needle {
				return lang, true
			}
		}
	}
	return Language{}, false
}

func (r Registry) IDSet(input string) (map[string]struct{}, error) {
	lang, ok := r.Resolve(input)
	if !ok {
		return nil, fmt.Errorf("unsupported language %q; use one of the allowed language ids or LLM names from CueForge /languages", input)
	}
	ids := make(map[string]struct{}, len(lang.IDs))
	for _, id := range lang.IDs {
		ids[strings.ToLower(id)] = struct{}{}
	}
	return ids, nil
}

func (r Registry) ConfiguredID(input string) (string, error) {
	lang, ok := r.Resolve(input)
	if !ok {
		return "", fmt.Errorf("unsupported language %q; use one of the allowed language ids or LLM names from CueForge /languages", input)
	}
	normalized := normalize(input)
	for _, id := range lang.IDs {
		if normalize(id) == normalized {
			return strings.ToLower(id), nil
		}
	}
	return strings.ToLower(lang.IDs[0]), nil
}

func normalize(input string) string {
	return strings.ToLower(strings.Join(strings.Fields(strings.TrimSpace(input)), " "))
}

package cueforge

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
)

func TestFetchLanguages(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/languages" {
			t.Fatalf("path = %s, want /languages", r.URL.Path)
		}
		_ = json.NewEncoder(w).Encode(LanguagesResponse{
			Languages: []Language{
				{IDs: []string{"fre", "fra"}, LLMName: "French"},
				{IDs: []string{"ger", "deu"}, LLMName: "German"},
			},
		})
	}))
	defer server.Close()

	registry, err := FetchLanguages(context.Background(), server.Client(), server.URL+"/")
	if err != nil {
		t.Fatalf("FetchLanguages failed: %v", err)
	}

	ids, err := registry.IDSet("ger")
	if err != nil {
		t.Fatalf("IDSet failed: %v", err)
	}
	if _, ok := ids["deu"]; !ok {
		t.Fatalf("German aliases = %#v, want deu", ids)
	}

	id, err := registry.ConfiguredID("French")
	if err != nil {
		t.Fatalf("ConfiguredID failed: %v", err)
	}
	if id != "fre" {
		t.Fatalf("ConfiguredID(French) = %q, want fre", id)
	}
}

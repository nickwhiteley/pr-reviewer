package provider

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"testing"
)

func TestOllamaProvider_endpointFromEnv(t *testing.T) {
	os.Setenv("OLLAMA_ENDPOINT", "https://custom.example.com")
	defer os.Unsetenv("OLLAMA_ENDPOINT")
	os.Setenv("OLLAMA_API_KEY", "key")
	defer os.Unsetenv("OLLAMA_API_KEY")

	o := NewOllamaProvider()
	if o.endpoint != "https://custom.example.com" {
		t.Errorf("expected endpoint from env, got %q", o.endpoint)
	}
}

func TestOllamaProvider_Generate_success(t *testing.T) {
	mux := http.NewServeMux()
	mux.HandleFunc("/api/chat", func(w http.ResponseWriter, r *http.Request) {
		if r.Method != "POST" {
			t.Errorf("expected POST, got %s", r.Method)
		}
		auth := r.Header.Get("Authorization")
		if auth != "Bearer test-key" {
			t.Errorf("expected Bearer test-key, got %s", auth)
		}
		json.NewEncoder(w).Encode(map[string]interface{}{
			"choices": []map[string]interface{}{
				{"message": map[string]string{"content": "review findings"}},
			},
		})
	})
	srv := httptest.NewServer(mux)
	defer srv.Close()

	os.Setenv("OLLAMA_API_KEY", "test-key")
	defer os.Unsetenv("OLLAMA_API_KEY")

	o := NewOllamaProvider()
	o.endpoint = srv.URL + "/api/chat"

	resp, err := o.Generate(context.Background(), "test-model", "prompt")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if resp.Text != "review findings" {
		t.Errorf("unexpected response: %q", resp.Text)
	}
}

func TestOllamaProvider_Generate_non2xx(t *testing.T) {
	mux := http.NewServeMux()
	mux.HandleFunc("/api/chat", func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusUnauthorized)
	})
	srv := httptest.NewServer(mux)
	defer srv.Close()

	os.Setenv("OLLAMA_API_KEY", "test-key")
	defer os.Unsetenv("OLLAMA_API_KEY")

	o := NewOllamaProvider()
	o.endpoint = srv.URL + "/api/chat"

	_, err := o.Generate(context.Background(), "", "prompt")
	if err == nil {
		t.Fatal("expected error for 401")
	}
}

func TestOllamaProvider_Generate_emptyResponse(t *testing.T) {
	mux := http.NewServeMux()
	mux.HandleFunc("/api/chat", func(w http.ResponseWriter, r *http.Request) {
		json.NewEncoder(w).Encode(map[string]interface{}{
			"choices": []map[string]interface{}{
				{"message": map[string]string{"content": ""}},
			},
		})
	})
	srv := httptest.NewServer(mux)
	defer srv.Close()

	os.Setenv("OLLAMA_API_KEY", "test-key")
	defer os.Unsetenv("OLLAMA_API_KEY")

	o := NewOllamaProvider()
	o.endpoint = srv.URL + "/api/chat"

	_, err := o.Generate(context.Background(), "", "prompt")
	if err == nil {
		t.Fatal("expected error for empty response")
	}
}

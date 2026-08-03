package provider

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
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

// Ollama silently truncates a prompt longer than num_ctx, so the request has
// to declare the context length it needs rather than inherit the server's
// default. A truncated prompt means the agent never sees the end of the diff
// but still reports as if it reviewed everything.
func TestOllamaProvider_Generate_setsNumCtx(t *testing.T) {
	var gotNumCtx float64
	mux := http.NewServeMux()
	mux.HandleFunc("/api/chat", func(w http.ResponseWriter, r *http.Request) {
		var payload struct {
			Options struct {
				NumCtx float64 `json:"num_ctx"`
			} `json:"options"`
		}
		if err := json.NewDecoder(r.Body).Decode(&payload); err != nil {
			t.Errorf("decode request: %v", err)
		}
		gotNumCtx = payload.Options.NumCtx
		json.NewEncoder(w).Encode(map[string]any{
			"choices": []map[string]any{{"message": map[string]string{"content": "ok"}}},
		})
	})
	srv := httptest.NewServer(mux)
	defer srv.Close()

	os.Setenv("OLLAMA_API_KEY", "test-key")
	defer os.Unsetenv("OLLAMA_API_KEY")

	o := NewOllamaProvider()
	o.endpoint = srv.URL + "/api/chat"

	prompt := strings.Repeat("x", 30000)
	if _, err := o.Generate(context.Background(), defaultModel, prompt); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	want := float64(len(prompt)/BytesPerToken + ResponseTokens)
	if gotNumCtx != want {
		t.Errorf("num_ctx = %v, want %v (prompt tokens plus response reserve)", gotNumCtx, want)
	}
}

func TestOllamaProvider_Generate_rejectsOversizedPrompt(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		t.Error("an over-window prompt must fail before reaching the provider")
	}))
	defer srv.Close()

	os.Setenv("OLLAMA_API_KEY", "test-key")
	defer os.Unsetenv("OLLAMA_API_KEY")

	o := NewOllamaProvider()
	o.endpoint = srv.URL + "/api/chat"

	// "unknown-model" falls back to the conservative default window.
	oversized := strings.Repeat("x", defaultContextTokens*BytesPerToken*2)
	_, err := o.Generate(context.Background(), "unknown-model", oversized)
	if err == nil {
		t.Fatal("expected an error rather than a silently truncated prompt")
	}
	if !strings.Contains(err.Error(), "window") {
		t.Errorf("error should explain the context window: %v", err)
	}
}

func TestOllamaPromptBudgetBytes(t *testing.T) {
	os.Setenv("OLLAMA_API_KEY", "key")
	defer os.Unsetenv("OLLAMA_API_KEY")
	o := NewOllamaProvider()

	// Kimi K2.6's 262,144-token window, minus the response reserve.
	want := (262144 - ResponseTokens) * BytesPerToken
	if got := o.PromptBudgetBytes("kimi-k2.6:cloud"); got != want {
		t.Errorf("kimi budget = %d, want %d", got, want)
	}
	// The empty model name resolves to the default model, not the fallback.
	if got := o.PromptBudgetBytes(""); got != want {
		t.Errorf("default budget = %d, want %d", got, want)
	}
	// An unrecognised model gets the conservative fallback.
	if got := o.PromptBudgetBytes("mystery"); got >= want {
		t.Errorf("unknown model budget = %d, expected below kimi's %d", got, want)
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

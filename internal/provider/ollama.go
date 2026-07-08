// Package provider implements the Ollama HTTP API provider.
package provider

import (
	"bytes"
	"context"
	"crypto/tls"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"time"
)

const defaultEndpoint = "https://ollama.com/api/chat"

// OllamaProvider sends prompts to the Ollama cloud API.
type OllamaProvider struct {
	client   *http.Client
	endpoint string
	apiKey   string
}

// NewOllamaProvider creates an Ollama provider from environment.
func NewOllamaProvider() *OllamaProvider {
	endpoint := os.Getenv("OLLAMA_ENDPOINT")
	if endpoint == "" {
		endpoint = defaultEndpoint
	}
	return &OllamaProvider{
		client: &http.Client{
			// Intentionally long timeout: LLM inference can take several minutes for
			// large diffs. We prefer slower execution over repeated timeout failures.
			Timeout: 600 * time.Second,
			Transport: &http.Transport{
				TLSNextProto: make(map[string]func(authority string, c *tls.Conn) http.RoundTripper),
			},
		},
		endpoint: endpoint,
		apiKey:   os.Getenv("OLLAMA_API_KEY"),
	}
}

// SetEndpoint overrides the endpoint URL (for testing).
func (o *OllamaProvider) SetEndpoint(url string) {
	o.endpoint = url
}

// Generate sends a prompt to Ollama and returns the response.
func (o *OllamaProvider) Generate(ctx context.Context, model string, prompt string) (Response, error) {
	if model == "" {
		model = "kimi-k2.6:cloud"
	}

	payload := map[string]interface{}{
		"model": model,
		"messages": []map[string]string{
			{"role": "user", "content": prompt},
		},
		"stream": false,
	}
	data, err := json.Marshal(payload)
	if err != nil {
		return Response{}, fmt.Errorf("marshal request: %w", err)
	}

	req, err := http.NewRequestWithContext(ctx, "POST", o.endpoint, bytes.NewReader(data))
	if err != nil {
		return Response{}, fmt.Errorf("create request: %w", err)
	}
	req.Header.Set("Authorization", "Bearer "+o.apiKey)
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json")
	req.Header.Set("User-Agent", "woodendollars-pr-review/1.0")

	resp, err := doWithRetry(o.client, req)
	if err != nil {
		return Response{}, fmt.Errorf("ollama generate: %w", err)
	}
	defer resp.Body.Close()

	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return Response{}, fmt.Errorf("read response: %w", err)
	}

	var openAIResult struct {
		Choices []struct {
			Message struct {
				Content string `json:"content"`
			} `json:"message"`
		} `json:"choices"`
	}
	var nativeResult struct {
		Message struct {
			Content string `json:"content"`
		} `json:"message"`
	}

	if err := json.Unmarshal(body, &openAIResult); err != nil {
		return Response{}, fmt.Errorf("decode response: %w", err)
	}
	if len(openAIResult.Choices) > 0 && openAIResult.Choices[0].Message.Content != "" {
		return Response{Text: openAIResult.Choices[0].Message.Content}, nil
	}

	if err := json.Unmarshal(body, &nativeResult); err != nil {
		return Response{}, fmt.Errorf("decode response: %w", err)
	}
	if nativeResult.Message.Content != "" {
		return Response{Text: nativeResult.Message.Content}, nil
	}

	return Response{}, fmt.Errorf("empty response from ollama")
}

// doWithRetry sends a request, retrying once on network errors and once on
// 5xx responses, with a fresh body each attempt. Shared by all providers.
func doWithRetry(client *http.Client, req *http.Request) (*http.Response, error) {
	doReq := func(r *http.Request) (*http.Response, error) {
		if r.GetBody != nil {
			body, err := r.GetBody()
			if err != nil {
				return nil, err
			}
			r.Body = body
		}
		return client.Do(r)
	}

	resp, err := doReq(req)
	if err != nil {
		// Retry once on network/timeout errors with a fresh request copy.
		retryReq := req.Clone(req.Context())
		if req.GetBody != nil {
			body, err := req.GetBody()
			if err != nil {
				return nil, err
			}
			retryReq.Body = body
		}
		resp, err = doReq(retryReq)
		if err != nil {
			return nil, err
		}
	}
	if resp.StatusCode >= 500 {
		// Retry once on server errors with a fresh request copy.
		body, _ := io.ReadAll(resp.Body)
		resp.Body.Close()
		retryReq := req.Clone(req.Context())
		if req.GetBody != nil {
			newBody, err := req.GetBody()
			if err != nil {
				return nil, err
			}
			retryReq.Body = newBody
		}
		resp, err = doReq(retryReq)
		if err != nil {
			return nil, fmt.Errorf("server error (retried): %w", err)
		}
		if resp.StatusCode >= 500 {
			return nil, fmt.Errorf("server error %d after retry", resp.StatusCode)
		}
		_ = body
	}
	if resp.StatusCode >= 400 {
		body, _ := io.ReadAll(resp.Body)
		return nil, fmt.Errorf("HTTP %d: %s", resp.StatusCode, string(body))
	}
	return resp, nil
}

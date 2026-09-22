// Package provider implements the Anthropic Messages API provider.
package provider

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"strings"
	"time"
)

const (
	anthropicEndpoint = "https://api.anthropic.com/v1/messages"
	anthropicVersion  = "2023-06-01"

	// AnthropicDefaultModel is the model used when --model names none. Exported
	// so the pin lives in one place: main.go used to carry a second copy of it,
	// and the two drifted.
	AnthropicDefaultModel = "claude-sonnet-5"

	// anthropicMaxTokens is the response cap; keep it and ResponseTokens in
	// step so the prompt budget reserves what the response may actually use.
	anthropicMaxTokens = 16384
)

// anthropicContextTokens is the context window, in tokens, of models we know
// about — the same shape the Ollama provider uses, and for the same reason:
// one constant cannot describe a lineup where Haiku is 200K and everything
// above it is 1M. The single 200000 that used to sit here was the Claude 3
// era, and it chunked every review about five times more finely than the
// window needed.
var anthropicContextTokens = map[string]int{
	"claude-opus-5":     1000000,
	"claude-sonnet-5":   1000000,
	"claude-sonnet-4-6": 1000000,
	"claude-haiku-4-5":  200000,
}

// anthropicDefaultContextTokens is the assumed window for an unrecognised
// model. Deliberately modest, matching the Ollama provider's reasoning:
// guessing low costs an extra chunk, guessing high invites a silently
// truncated prompt.
const anthropicDefaultContextTokens = 200000

// contextTokensFor returns the context window for a model, ignoring any
// dated-snapshot suffix if the exact name isn't known.
func contextTokensFor(model string) int {
	if model == "" {
		model = AnthropicDefaultModel
	}
	if n, ok := anthropicContextTokens[model]; ok {
		return n
	}
	// "claude-haiku-4-5-20251001" and the like resolve to their base name.
	for base, n := range anthropicContextTokens {
		if strings.HasPrefix(model, base+"-") {
			return n
		}
	}
	return anthropicDefaultContextTokens
}

// PromptBudgetBytes implements Provider.
func (a *AnthropicProvider) PromptBudgetBytes(model string) int {
	return (contextTokensFor(model) - anthropicMaxTokens) * BytesPerToken
}

// AnthropicProvider sends prompts to the Anthropic Messages API.
type AnthropicProvider struct {
	client *http.Client
	apiKey string
}

// NewAnthropicProvider creates an Anthropic provider from environment (ANTHROPIC_API_KEY).
func NewAnthropicProvider() *AnthropicProvider {
	return &AnthropicProvider{
		client: &http.Client{Timeout: 300 * time.Second},
		apiKey: os.Getenv("ANTHROPIC_API_KEY"),
	}
}

// Generate sends a prompt to the Anthropic Messages API and returns the response.
func (a *AnthropicProvider) Generate(ctx context.Context, model string, prompt string) (Response, error) {
	if model == "" {
		model = AnthropicDefaultModel
	}

	payload := map[string]any{
		"model":      model,
		"max_tokens": anthropicMaxTokens,
		"messages": []map[string]string{
			{"role": "user", "content": prompt},
		},
	}
	data, err := json.Marshal(payload)
	if err != nil {
		return Response{}, fmt.Errorf("marshal request: %w", err)
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, anthropicEndpoint, bytes.NewReader(data))
	if err != nil {
		return Response{}, fmt.Errorf("create request: %w", err)
	}
	req.Header.Set("x-api-key", a.apiKey)
	req.Header.Set("anthropic-version", anthropicVersion)
	req.Header.Set("content-type", "application/json")

	resp, err := doWithRetry(a.client, req)
	if err != nil {
		return Response{}, fmt.Errorf("anthropic generate: %w", err)
	}
	defer resp.Body.Close()

	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return Response{}, fmt.Errorf("read response: %w", err)
	}

	var result struct {
		StopReason string `json:"stop_reason"`
		Content    []struct {
			Type string `json:"type"`
			Text string `json:"text"`
		} `json:"content"`
	}
	if err := json.Unmarshal(body, &result); err != nil {
		return Response{}, fmt.Errorf("decode response: %w", err)
	}
	if result.StopReason == "max_tokens" {
		// A truncated report may have lost findings or its severity block;
		// failing here routes into the agent-failure path instead of a
		// silently incomplete review.
		return Response{}, fmt.Errorf("anthropic response truncated at max_tokens — report would be incomplete")
	}
	var text strings.Builder
	for _, block := range result.Content {
		if block.Type == "text" {
			text.WriteString(block.Text)
		}
	}
	if text.Len() == 0 {
		return Response{}, fmt.Errorf("empty response from anthropic")
	}
	return Response{Text: text.String()}, nil
}

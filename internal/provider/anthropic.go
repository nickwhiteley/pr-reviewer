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
)

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
		model = "claude-sonnet-4-6"
	}

	payload := map[string]any{
		"model":      model,
		"max_tokens": 8096,
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

// Package provider defines the AI provider interface.
package provider

import "context"

// Provider is the abstraction for AI backends (Ollama, Anthropic, etc.).
type Provider interface {
	// Generate sends a prompt to the AI and returns the response text.
	Generate(ctx context.Context, model string, prompt string) (Response, error)
}

// Response holds the raw AI response.
type Response struct {
	Text string
}

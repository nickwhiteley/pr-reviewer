// Package provider defines the AI provider interface.
package provider

import "context"

// Provider is the abstraction for AI backends (Ollama, Anthropic, etc.).
type Provider interface {
	// Generate sends a prompt to the AI and returns the response text.
	Generate(ctx context.Context, model string, prompt string) (Response, error)

	// PromptBudgetBytes returns roughly how many bytes of prompt the given
	// model can accept, after reserving room for the response. Diff chunking
	// is sized against it so chunks stay comfortably inside the window
	// instead of being fixed at a constant that may not match the model.
	PromptBudgetBytes(model string) int
}

// BytesPerToken is the rough bytes-per-token ratio used to convert model
// context windows (quoted in tokens) into prompt byte budgets. Diff and code
// text tokenizes denser than prose — around 3 to 4 bytes per token — so 3 is
// the conservative end, which is what we want when the consequence of
// underestimating is a silently truncated prompt.
const BytesPerToken = 3

// ResponseTokens is the room reserved for the model's own output when
// computing a prompt budget or a context length.
const ResponseTokens = 8192

// Response holds the raw AI response.
type Response struct {
	Text string
}

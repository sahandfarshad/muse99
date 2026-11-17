package main

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"
)

// OpenAI-compatible Chat Completions client.
// Works with: Meta Model API (Spark), OpenAI, OpenRouter, Ollama,
// LM Studio, Groq, Together, etc. — anything that speaks /chat/completions.

type ToolCall struct {
	ID       string `json:"id"`
	Type     string `json:"type"`
	Function struct {
		Name      string `json:"name"`
		Arguments string `json:"arguments"`
	} `json:"function"`
	// ExtraContent carries provider-specific extras that the OpenAI
	// schema has no room for. Gemini 3/4 return their reasoning
	// signature here — extra_content.google.thought_signature — and
	// reject the next turn with 400 ("Function call is missing a
	// thought_signature") if it is not sent back verbatim. Standard
	// OpenAI clients strip unknown fields, which is exactly how the
	// signature gets lost; keeping it on the struct keeps it in the
	// replayed history, the saved session and the chat DB.
	ExtraContent *ToolCallExtra `json:"extra_content,omitempty"`
	// ThoughtSignature is the flat variant some gateways use instead
	// of extra_content. Preserved the same way.
	ThoughtSignature string `json:"thought_signature,omitempty"`
}

// ToolCallExtra is the provider-specific payload on a tool call.
type ToolCallExtra struct {
	Google struct {
		ThoughtSignature string `json:"thought_signature,omitempty"`
	} `json:"google,omitempty"`
}

// thoughtSignature returns the reasoning signature carried by this
// tool call, from either wire location, or "" when there is none.
func (tc *ToolCall) thoughtSignature() string {
	if tc.ExtraContent != nil && tc.ExtraContent.Google.ThoughtSignature != "" {
		return tc.ExtraContent.Google.ThoughtSignature
	}
	return tc.ThoughtSignature
}

// setThoughtSignature stores sig in the documented
// extra_content.google.thought_signature location.
func (tc *ToolCall) setThoughtSignature(sig string) {
	if tc.ExtraContent == nil {
		tc.ExtraContent = &ToolCallExtra{}
	}
	tc.ExtraContent.Google.ThoughtSignature = sig
}

// skipThoughtSignatureValidator is Google's documented last-resort
// value for a functionCall part the API did not generate (for
// example history from a model that does not produce signatures, or
// a session saved before signatures were preserved). Google says to
// use it only as a last resort — it degrades model performance —
// so it is only filled in where a signature is genuinely missing,
// never over a real one.
const skipThoughtSignatureValidator = "skip_thought_signature_validator"

// needsThoughtSignature reports whether the current model strictly
// validates thought signatures on tool calls. Gemini 3 and 4 do;
// Gemini 2.5 and earlier do not return signatures and do not
// validate them, so nothing is injected for those.
func (c *Client) needsThoughtSignature() bool {
	if !isGeminiBase(c.BaseURL, c.Provider) {
		return false
	}
	m := strings.ToLower(c.Model)
	return strings.Contains(m, "gemini-3") || strings.Contains(m, "gemini-4")
}

// ensureThoughtSignatures makes sure every assistant message in the
// outgoing history carries a signature on its FIRST tool call when
// the model requires one. Only the first call of a turn carries a
// signature — parallel siblings arrive unsigned and must stay
// unsigned — so later calls in the same message are left alone.
// Messages that already carry a real signature are untouched.
func ensureThoughtSignatures(msgs []Message) {
	for i := range msgs {
		if msgs[i].Role != "assistant" || len(msgs[i].ToolCalls) == 0 {
			continue
		}
		if msgs[i].ToolCalls[0].thoughtSignature() == "" {
			msgs[i].ToolCalls[0].setThoughtSignature(skipThoughtSignatureValidator)
		}
	}
}

type Message struct {
	Role       string     `json:"role"`
	Content    string     `json:"content,omitempty"`
	ToolCalls  []ToolCall `json:"tool_calls,omitempty"`
	ToolCallID string     `json:"tool_call_id,omitempty"`
	Name       string     `json:"name,omitempty"`
}

type ToolDef struct {
	Type     string `json:"type"`
	Function struct {
		Name        string         `json:"name"`
		Description string         `json:"description"`
		Parameters  map[string]any `json:"parameters"`
	} `json:"function"`
}

type chatRequest struct {
	Model       string    `json:"model"`
	Messages    []Message `json:"messages"`
	Tools       []ToolDef `json:"tools,omitempty"`
	Temperature float64   `json:"temperature,omitempty"`
	MaxTokens   int       `json:"max_completion_tokens,omitempty"`
}

type chatResponse struct {
	Choices []struct {
		Message Message `json:"message"`
	} `json:"choices"`
	Usage *struct {
		PromptTokens     int `json:"prompt_tokens"`
		CompletionTokens int `json:"completion_tokens"`
		TotalTokens      int `json:"total_tokens"`
	} `json:"usage,omitempty"`
	Error *struct {
		Message string `json:"message"`
		Type    string `json:"type"`
		Code    string `json:"code"`
	} `json:"error,omitempty"`
}

type Client struct {
	BaseURL  string
	APIKey   string
	Model    string
	Provider string // v6: the provider name, so auth quirks (Gemini's
	// x-goog-api-key retry) work even when the base URL is a test
	// server or a custom endpoint that does not reveal the provider.
	HTTP *http.Client
}

func NewClient(baseURL, apiKey, model string) *Client {
	return NewClientForProvider(baseURL, apiKey, model, "")
}

// NewClientForProvider is NewClient with an explicit provider name.
// An empty provider is guessed from the base URL / model.
func NewClientForProvider(baseURL, apiKey, model, provider string) *Client {
	baseURL = strings.TrimRight(strings.TrimSpace(baseURL), "/")
	if baseURL == "" {
		baseURL = "https://api.meta.ai/v1"
	}
	// v6: a model id saved with a "models/" prefix (Gemini's native
	// list format) would fail every chat with "model not found" —
	// always send the bare id.
	model = normalizeModelID(model)
	if model == "" {
		model = "muse-spark-1.3"
	}
	if provider == "" {
		provider = matchProvider(baseURL, model)
	}
	return &Client{
		BaseURL:  baseURL,
		APIKey:   apiKey,
		Model:    model,
		Provider: provider,
		HTTP:     &http.Client{Timeout: 120 * time.Second},
	}
}

func (c *Client) Chat(messages []Message, tools []ToolDef) (Message, error) {
	// Gemini 3/4 thought signatures: a tool call the model made must
	// come back with its signature, or the next turn 400s. Fill the
	// documented bypass only where a signature is genuinely missing
	// (history from another model, or a session saved before
	// signatures were preserved). This mutates the caller's messages
	// in place on purpose — the stored history then replays with the
	// signature/sentinel too, instead of 400ing on every retry.
	if c.needsThoughtSignature() {
		ensureThoughtSignatures(messages)
	}
	reqBody := chatRequest{
		Model:       c.Model,
		Messages:    messages,
		Tools:       tools,
		Temperature: 0.7,
		MaxTokens:   4096,
	}
	// Spark reasons before answering; those tokens count toward the limit.
	if isSpark(c.BaseURL, c.Model) {
		reqBody.MaxTokens = 8192
	}

	data, err := json.Marshal(reqBody)
	if err != nil {
		return Message{}, err
	}

	url := c.BaseURL + "/chat/completions"
	// doChat sends the request once. useGoogKey sends the key as
	// x-goog-api-key instead of Bearer (see below).
	doChat := func(useGoogKey bool) ([]byte, int, error) {
		req, err := http.NewRequest("POST", url, bytes.NewReader(data))
		if err != nil {
			return nil, 0, err
		}
		req.Header.Set("Content-Type", "application/json")
		if c.APIKey != "" {
			if useGoogKey {
				req.Header.Set("x-goog-api-key", c.APIKey)
			} else {
				req.Header.Set("Authorization", "Bearer "+c.APIKey)
			}
		}
		resp, err := c.HTTP.Do(req)
		if err != nil {
			return nil, 0, fmt.Errorf("api request failed: %w", err)
		}
		defer resp.Body.Close()
		body, _ := io.ReadAll(resp.Body)
		return body, resp.StatusCode, nil
	}

	body, statusCode, err := doChat(false)
	if err != nil {
		return Message{}, err
	}
	// v6 Gemini auth: some Gemini keys are rejected as Bearer (401/403)
	// on the OpenAI-compatible endpoint but accepted with the native
	// x-goog-api-key header — retry once that way before giving up.
	if (statusCode == 401 || statusCode == 403) && c.APIKey != "" &&
		(c.Provider == "gemini" || strings.Contains(strings.ToLower(c.BaseURL), "generativelanguage.googleapis.com")) {
		if retryBody, retryStatus, rerr := doChat(true); rerr == nil && retryStatus < 400 {
			body, statusCode = retryBody, retryStatus
		}
	}
	var out chatResponse
	if err := json.Unmarshal(body, &out); err != nil {
		return Message{}, fmt.Errorf("bad api response (%d): %s", statusCode, string(body)[:min(500, len(body))])
	}
	if out.Error != nil {
		return Message{}, fmt.Errorf("api error: %s", out.Error.Message)
	}
	if statusCode >= 400 {
		return Message{}, fmt.Errorf("api error %d: %s", statusCode, string(body)[:min(500, len(body))])
	}
	if len(out.Choices) == 0 {
		return Message{}, fmt.Errorf("api returned no choices")
	}
	// Token usage: capture it for every call (some providers/local
	// models omit it — then nothing is recorded for that call).
	if out.Usage != nil {
		recordUsage(out.Usage.PromptTokens, out.Usage.CompletionTokens)
	}
	return out.Choices[0].Message, nil
}

func min(a, b int) int {
	if a < b {
		return a
	}
	return b
}

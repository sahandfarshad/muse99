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
}

type Message struct {
	Role      string     `json:"role"`
	Content   string     `json:"content,omitempty"`
	ToolCalls []ToolCall `json:"tool_calls,omitempty"`
	ToolCallID string    `json:"tool_call_id,omitempty"`
	Name      string     `json:"name,omitempty"`
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
	Model          string    `json:"model"`
	Messages       []Message `json:"messages"`
	Tools          []ToolDef `json:"tools,omitempty"`
	Temperature    float64   `json:"temperature,omitempty"`
	MaxTokens      int       `json:"max_completion_tokens,omitempty"`
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

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
	BaseURL string
	APIKey  string
	Model   string
	HTTP    *http.Client
}

func NewClient(baseURL, apiKey, model string) *Client {
	baseURL = strings.TrimRight(strings.TrimSpace(baseURL), "/")
	if baseURL == "" {
		baseURL = "https://api.meta.ai/v1"
	}
	if model == "" {
		model = "muse-spark-1.3"
	}
	return &Client{
		BaseURL: baseURL,
		APIKey:  apiKey,
		Model:   model,
		HTTP:    &http.Client{Timeout: 120 * time.Second},
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
	req, err := http.NewRequest("POST", url, bytes.NewReader(data))
	if err != nil {
		return Message{}, err
	}
	req.Header.Set("Content-Type", "application/json")
	if c.APIKey != "" {
		req.Header.Set("Authorization", "Bearer "+c.APIKey)
	}

	resp, err := c.HTTP.Do(req)
	if err != nil {
		return Message{}, fmt.Errorf("api request failed: %w", err)
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)

	var out chatResponse
	if err := json.Unmarshal(body, &out); err != nil {
		return Message{}, fmt.Errorf("bad api response (%d): %s", resp.StatusCode, string(body)[:min(500, len(body))])
	}
	if out.Error != nil {
		return Message{}, fmt.Errorf("api error: %s", out.Error.Message)
	}
	if resp.StatusCode >= 400 {
		return Message{}, fmt.Errorf("api error %d: %s", resp.StatusCode, string(body)[:min(500, len(body))])
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

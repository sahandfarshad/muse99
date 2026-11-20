package main

import (
	"bytes"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
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
	// Reasoning is the model's visible thinking (reasoning_content /
	// reasoning), shown in the activity panel only; never sent back.
	Reasoning string `json:"-"`
	// Context (v0.9.10) is extra text sent with this message only to
	// the model (the memories picked for it), never stored or shown.
	// It sits on the user message instead of in the system prompt, so
	// the start of every request stays the same and providers can
	// cache it.
	Context string `json:"-"`
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
		Message      Message `json:"message"`
		FinishReason string  `json:"finish_reason"`
	} `json:"choices"`
	Usage *struct {
		PromptTokens        int `json:"prompt_tokens"`
		CompletionTokens    int `json:"completion_tokens"`
		TotalTokens         int `json:"total_tokens"`
		PromptTokensDetails *struct {
			CachedTokens int `json:"cached_tokens"`
		} `json:"prompt_tokens_details,omitempty"`
		PromptCacheHitTokens int `json:"prompt_cache_hit_tokens"` // DeepSeek
		CachedTokens         int `json:"cached_tokens"`
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
	// flattenTools: this provider rejected native tool history once,
	// so tool steps are sent as plain text from then on.
	flattenTools bool
	// NoRetry: fail at once instead of waiting out network or
	// provider hiccups (model tests).
	NoRetry bool
	// MaxTokens overrides the reply budget for this client (0 = the
	// default, MUSE99_MAX_TOKENS or 8192). Set lower automatically when
	// a marketplace router (AiPrime, OpenRouter) refuses the request
	// because the worst-case price of that many tokens is too high.
	MaxTokens int
	// noMaxTokens: the provider only accepted the request without a
	// max token field at all.
	noMaxTokens bool
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
		HTTP:     &http.Client{Timeout: modelTimeout()},
	}
}

func (c *Client) chatOnce(messages []Message, tools []ToolDef) (Message, error) {
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
	// v0.8: some providers (GLM, some local models) return tool calls
	// without an id. That history then breaks every other provider
	// (OpenRouter: "tool messages must include a non-empty string
	// tool_call_id", Gemini: INVALID_ARGUMENT). Repair ids in place,
	// and send a cleaned copy where tool calls and results pair up.
	fillToolCallIDs(messages)
	sent := clipOldToolResults(sanitizeToolHistory(withContext(messages)))
	if isGeminiBase(c.BaseURL, c.Provider) {
		nameToolResults(sent)
	}
	reqBody := chatRequest{
		Model:       c.Model,
		Messages:    sent,
		Tools:       tools,
		Temperature: 0.7,
		MaxTokens:   maxTokens(),
	}
	// Spark reasons before answering; those tokens count toward the limit.
	if isSpark(c.BaseURL, c.Model) && reqBody.MaxTokens < 8192 {
		reqBody.MaxTokens = 8192
	}
	if c.MaxTokens > 0 {
		reqBody.MaxTokens = c.MaxTokens
	}
	if c.noMaxTokens {
		reqBody.MaxTokens = 0
	}

	if c.flattenTools && hasToolSteps(sent) {
		reqBody.Messages = flattenToolHistory(sent)
	}
	data, err := json.Marshal(reqBody)
	if err != nil {
		return Message{}, err
	}

	url := c.BaseURL + "/chat/completions"
	// doChat sends the request once. useGoogKey sends the key as
	// x-goog-api-key instead of Bearer (see below).
	doChat := func(useGoogKey bool) ([]byte, int, error) {
		req, err := http.NewRequestWithContext(ctl.context(), "POST", url, bytes.NewReader(data))
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
	// v0.8: a 400 on a history with tool calls is almost always the
	// provider rejecting how the tool steps are encoded (ids, names,
	// signatures — each provider has its own rules, and history may
	// come from another model). Save the request for debugging, then
	// retry once with the tool steps written out as plain text, which
	// every provider accepts. The model can still call tools again.
	if statusCode == 400 && !c.flattenTools && hasToolSteps(sent) {
		saveBadRequest(reqBody, body)
		activity.add("", "retry", "Provider rejected the tool history (400) — retrying with tool steps as plain text", truncate(string(body), 600), false)
		reqBody.Messages = flattenToolHistory(sent)
		if d, merr := json.Marshal(reqBody); merr == nil {
			data = d
			if rb, rs, rerr := doChat(false); rerr == nil {
				body, statusCode = rb, rs
				if rs < 400 {
					c.flattenTools = true // keep doing this for this provider
				}
			}
		}
	}
	// v0.9: marketplace routers price a request by its worst case
	// (input + max tokens). With a big budget they answer "No seller
	// can serve this model at an affordable price" (or 402 / "more
	// credits") even though a normal request works. Retry with a
	// smaller budget, then with none, and keep what worked.
	if statusCode >= 400 && looksLikeBudgetError(statusCode, body) && reqBody.MaxTokens > 0 {
		for _, n := range []int{2048} {
			if n != 0 && n >= reqBody.MaxTokens {
				continue
			}
			activity.add("", "retry", fmt.Sprintf("Provider refused the price of %d max tokens — retrying with %s", reqBody.MaxTokens, budgetLabel(n)), truncate(string(body), 400), false)
			reqBody.MaxTokens = n
			d, merr := json.Marshal(reqBody)
			if merr != nil {
				break
			}
			data = d
			rb, rs, rerr := doChat(false)
			if rerr != nil {
				break
			}
			body, statusCode = rb, rs
			if rs < 400 {
				if n == 0 {
					c.noMaxTokens = true
				} else {
					c.MaxTokens = n
				}
				break
			}
			if !looksLikeBudgetError(rs, rb) {
				break
			}
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
		recordUsageFor(c.Provider, c.Model, out.Usage.PromptTokens, out.Usage.CompletionTokens)
		cached := out.Usage.CachedTokens
		if out.Usage.PromptTokensDetails != nil && out.Usage.PromptTokensDetails.CachedTokens > cached {
			cached = out.Usage.PromptTokensDetails.CachedTokens
		}
		if out.Usage.PromptCacheHitTokens > cached {
			cached = out.Usage.PromptCacheHitTokens
		}
		recordCached(c.Model, cached)
	}
	msg := out.Choices[0].Message
	var rs struct {
		Choices []struct {
			Message struct {
				ReasoningContent string `json:"reasoning_content"`
				Reasoning        string `json:"reasoning"`
			} `json:"message"`
		} `json:"choices"`
	}
	if json.Unmarshal(body, &rs) == nil && len(rs.Choices) > 0 {
		msg.Reasoning = rs.Choices[0].Message.ReasoningContent
		if msg.Reasoning == "" {
			msg.Reasoning = rs.Choices[0].Message.Reasoning
		}
	}
	if strings.TrimSpace(msg.Content) == "" && len(msg.ToolCalls) == 0 && out.Choices[0].FinishReason == "length" {
		return Message{}, fmt.Errorf("the model used its whole token budget (%d) thinking and wrote no answer — raise MUSE99_MAX_TOKENS or pick a non-reasoning model", reqBody.MaxTokens)
	}
	return msg, nil
}

// maxTokens is the most a model may write in one reply, thinking
// included. Reasoning models spend a lot of it before answering, so
// the default is 8192. Override with MUSE99_MAX_TOKENS (256..200000).
func maxTokens() int {
	n := 8192
	if v := strings.TrimSpace(os.Getenv("MUSE99_MAX_TOKENS")); v != "" {
		if x, err := strconv.Atoi(v); err == nil && x >= 256 && x <= 200000 {
			n = x
		}
	}
	return n
}

// MarshalJSON always sends content for assistant messages with tool
// calls (as ""), since some providers reject a missing content field
// ("text content parts must carry a string text").
func (m Message) MarshalJSON() ([]byte, error) {
	type plain Message
	if m.Role == "assistant" && len(m.ToolCalls) > 0 && m.Content == "" {
		return json.Marshal(struct {
			plain
			Content string `json:"content"`
		}{plain(m), ""})
	}
	return json.Marshal(plain(m))
}

func min(a, b int) int {
	if a < b {
		return a
	}
	return b
}

// modelTimeout is how long one model call may take. Reasoning models
// (GLM, DeepSeek, o-series) can think for many minutes before the first
// byte, so the default is 15 minutes. Override with MUSE99_MODEL_TIMEOUT
// (seconds, 30..7200).
func modelTimeout() time.Duration {
	d := 15 * time.Minute
	if v := strings.TrimSpace(os.Getenv("MUSE99_MODEL_TIMEOUT")); v != "" {
		if n, err := strconv.Atoi(v); err == nil && n >= 30 && n <= 7200 {
			d = time.Duration(n) * time.Second
		}
	}
	return d
}

// newToolCallID makes an id for a tool call the provider left blank.
func newToolCallID() string {
	b := make([]byte, 8)
	_, _ = rand.Read(b)
	return "call_" + hex.EncodeToString(b)
}

// fillToolCallIDs gives every tool call an id and every tool result
// the id of the call it answers (results follow their assistant
// message in order). It changes messages in place, so the stored chat
// is repaired too. The slice length never changes.
func fillToolCallIDs(messages []Message) {
	var pending []string
	for i := range messages {
		m := &messages[i]
		switch m.Role {
		case "assistant":
			pending = pending[:0]
			for j := range m.ToolCalls {
				if strings.TrimSpace(m.ToolCalls[j].ID) == "" {
					m.ToolCalls[j].ID = newToolCallID()
				}
				if m.ToolCalls[j].Type == "" {
					m.ToolCalls[j].Type = "function"
				}
				pending = append(pending, m.ToolCalls[j].ID)
			}
		case "tool":
			if strings.TrimSpace(m.ToolCallID) == "" && len(pending) > 0 {
				m.ToolCallID = pending[0]
			}
			// Consume the id this result answers.
			for k, id := range pending {
				if id == m.ToolCallID {
					pending = append(pending[:k], pending[k+1:]...)
					break
				}
			}
		default:
			pending = pending[:0]
		}
	}
}

// sanitizeToolHistory returns a copy of messages that every
// OpenAI-compatible provider accepts:
//   - a tool result whose call is not in the assistant message right
//     before it (or has no id) becomes a plain user note;
//   - a tool call that never got a result gets a short placeholder
//     result, so the call/result pairs are complete;
//   - tool results come straight after their assistant message.
func sanitizeToolHistory(messages []Message) []Message {
	out := make([]Message, 0, len(messages)+2)
	var open map[string]bool // calls of the last assistant message without a result yet
	var openOrder []string
	closeOpen := func() {
		for _, id := range openOrder {
			if open[id] {
				out = append(out, Message{Role: "tool", ToolCallID: id, Content: "(no result — the step was interrupted)"})
			}
		}
		open, openOrder = nil, nil
	}
	for _, m := range messages {
		switch m.Role {
		case "tool":
			if m.ToolCallID != "" && open[m.ToolCallID] {
				out = append(out, m)
				open[m.ToolCallID] = false
				continue
			}
			// Orphan result: keep the information, drop the tool role.
			closeOpen()
			out = append(out, Message{Role: "user", Content: "[earlier tool result]\n" + m.Content})
		case "assistant":
			closeOpen()
			if len(m.ToolCalls) == 0 && strings.TrimSpace(m.Content) == "" {
				continue // an empty reply stored earlier: providers reject it
			}
			if len(m.ToolCalls) > 0 {
				open = map[string]bool{}
				for _, tc := range m.ToolCalls {
					open[tc.ID] = true
					openOrder = append(openOrder, tc.ID)
				}
			}
			out = append(out, m)
		default:
			closeOpen()
			if (m.Role == "user" || m.Role == "assistant") && strings.TrimSpace(m.Content) == "" {
				continue // an empty reply stored earlier: providers reject it
			}
			out = append(out, m)
		}
	}
	closeOpen()
	return out
}

// hasToolSteps reports whether a history carries tool calls or results.
func hasToolSteps(msgs []Message) bool {
	for _, m := range msgs {
		if m.Role == "tool" || len(m.ToolCalls) > 0 {
			return true
		}
	}
	return false
}

// nameToolResults sets each tool result's name to the function it
// answers (Gemini's OpenAI layer maps results back to calls by name).
func nameToolResults(msgs []Message) {
	names := map[string]string{}
	for i := range msgs {
		for _, tc := range msgs[i].ToolCalls {
			names[tc.ID] = tc.Function.Name
		}
		if msgs[i].Role == "tool" && msgs[i].Name == "" {
			msgs[i].Name = names[msgs[i].ToolCallID]
		}
		if msgs[i].Role == "tool" && msgs[i].Content == "" {
			msgs[i].Content = "(no output)"
		}
	}
}

// flattenToolHistory writes tool calls and results as plain text:
// an assistant message says which tools it called, and each result
// becomes a user message. No tool_calls / tool roles remain.
func flattenToolHistory(msgs []Message) []Message {
	out := make([]Message, 0, len(msgs))
	names := map[string]string{}
	for _, m := range msgs {
		switch {
		case m.Role == "assistant" && len(m.ToolCalls) > 0:
			var b strings.Builder
			b.WriteString(strings.TrimSpace(m.Content))
			for _, tc := range m.ToolCalls {
				names[tc.ID] = tc.Function.Name
				if b.Len() > 0 {
					b.WriteString("\n")
				}
				fmt.Fprintf(&b, "[I called the tool %s with %s]", tc.Function.Name, truncate(tc.Function.Arguments, 4000))
			}
			out = append(out, Message{Role: "assistant", Content: b.String()})
		case m.Role == "tool":
			n := names[m.ToolCallID]
			if n == "" {
				n = "tool"
			}
			c := m.Content
			if c == "" {
				c = "(no output)"
			}
			out = append(out, Message{Role: "user", Content: "[result of " + n + "]\n" + c})
		default:
			m.ToolCalls, m.ToolCallID, m.Name = nil, "", ""
			out = append(out, m)
		}
	}
	return out
}

// saveBadRequest keeps the last rejected request (no API key — that
// is only in the headers) in ~/.muse99/data/last_bad_request.json so a
// provider error can be diagnosed. Long texts are cut short.
func saveBadRequest(req chatRequest, resp []byte) {
	cp := make([]Message, len(req.Messages))
	copy(cp, req.Messages)
	for i := range cp {
		cp[i].Content = truncate(cp[i].Content, 1500)
	}
	dump := map[string]any{"model": req.Model, "messages": cp, "tool_count": len(req.Tools), "response": string(resp[:min(2000, len(resp))])}
	if b, err := json.MarshalIndent(dump, "", "  "); err == nil {
		dir := filepath.Join(configDir(), "data")
		_ = os.MkdirAll(dir, 0o700)
		_ = os.WriteFile(filepath.Join(dir, "last_bad_request.json"), b, 0o600)
	}
}

// looksLikeBudgetError: the provider refused because of price or
// credit for the requested token budget, not because of the request.
func looksLikeBudgetError(status int, body []byte) bool {
	if status == 402 {
		return true
	}
	b := strings.ToLower(string(body))
	for _, k := range []string{"affordable", "more credits", "can only afford", "insufficient credit", "insufficient balance", "max_tokens", "max_completion_tokens", "maximum context length"} {
		if strings.Contains(b, k) {
			return true
		}
	}
	return false
}

func budgetLabel(n int) string {
	if n == 0 {
		return "no max token limit"
	}
	return fmt.Sprintf("%d max tokens", n)
}

// retryDelays is how long Chat waits before each retry when the
// connection drops or the provider is busy. About 4 minutes in all,
// so a short outage does not throw away a long task.
var retryDelays = []time.Duration{5 * time.Second, 15 * time.Second, 30 * time.Second, 60 * time.Second, 120 * time.Second}

// Chat sends one request, waiting and retrying through network drops,
// timeouts, rate limits, 5xx and "no seller right now" answers.
// A real error (bad key, bad request) returns at once.
func (c *Client) Chat(messages []Message, tools []ToolDef) (Message, error) {
	for attempt := 0; ; attempt++ {
		if err := paceWait(); err != nil {
			return Message{}, err
		}
		if ctl.context().Err() != nil {
			return Message{}, errStopped
		}
		msg, err := c.chatOnce(messages, tools)
		paceDone()
		if err != nil && ctl.context().Err() != nil {
			return Message{}, errStopped
		}
		if err == nil || c.NoRetry || !isTransientError(err) || attempt >= len(retryDelays) {
			if err != nil && attempt > 0 {
				err = fmt.Errorf("%w (gave up after %d retries)", err, attempt)
			}
			return msg, err
		}
		d := retryDelays[attempt]
		id := activity.add("", "retry", fmt.Sprintf("Connection or provider problem — waiting %s, then retry %d of %d…", d, attempt+1, len(retryDelays)), err.Error(), true)
		if sleepCtx(d) != nil {
			activity.finish(id, false, "Stopped while waiting to retry", "")
			return Message{}, errStopped
		}
		activity.finish(id, true, fmt.Sprintf("Waited %s, retrying (%d of %d)", d, attempt+1, len(retryDelays)), "")
	}
}

// isTransientError: worth waiting and trying again.
func isTransientError(err error) bool {
	e := strings.ToLower(err.Error())
	for _, k := range []string{"connection reset", "connection refused", "broken pipe", "eof", "timeout", "deadline exceeded",
		"no such host", "temporary failure", "tls handshake", "network is unreachable",
		"api error 429", "api error 500", "api error 502", "api error 503", "api error 504", "api error 520", "api error 522", "api error 524", "api error 529",
		"rate limit", "overloaded", "try again", "retry in a minute", "affordable", "server is busy", "bad api response (5", "bad api response (429"} {
		if strings.Contains(e, k) {
			return true
		}
	}
	return false
}

// clipOldToolResults shortens long tool outputs except the last few,
// on the copy that is sent (the saved history keeps them whole).
// Every request re-sends the whole conversation, so a long task paid
// for old file dumps again on every step.
func clipOldToolResults(msgs []Message) []Message {
	const keepLast = 8
	const maxOld = 1500
	out := make([]Message, len(msgs))
	copy(out, msgs)
	for i := 0; i < len(out)-keepLast; i++ {
		if out[i].Role == "tool" && len(out[i].Content) > maxOld {
			out[i].Content = out[i].Content[:maxOld] + fmt.Sprintf("\n…[%d more characters cut from this old tool output to save tokens; run the tool again if you need it]", len(out[i].Content)-maxOld)
		}
	}
	return out
}

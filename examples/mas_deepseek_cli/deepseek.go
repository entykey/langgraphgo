package main

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"
)

const (
	deepseekModel = "deepseek-v4-flash"
	// canonical endpoint per DeepSeek docs; /v1/chat/completions also works
	deepseekAPI = "https://api.deepseek.com/chat/completions"
)

// ── internal OpenAI-compatible API types ─────────────────────────────────────

type dsMsg struct {
	Role    string `json:"role"`
	Content string `json:"content"`
}

// thinkingConfig disables DeepSeek's chain-of-thought (thinking is ON by default
// for deepseek-v4-flash, which inflates token count and latency significantly).
type thinkingConfig struct {
	Type string `json:"type"`
}

type dsStreamOptions struct {
	IncludeUsage bool `json:"include_usage"`
}

type dsRequest struct {
	Model          string            `json:"model"`
	Messages       []dsMsg           `json:"messages"`
	Stream         bool              `json:"stream,omitempty"`
	ResponseFormat map[string]string `json:"response_format,omitempty"`
	MaxTokens      int               `json:"max_tokens,omitempty"`
	Temperature    float64           `json:"temperature"`
	Thinking       thinkingConfig    `json:"thinking"`
	StreamOptions  *dsStreamOptions  `json:"stream_options,omitempty"`
}

type dsUsage struct {
	PromptTokens     int `json:"prompt_tokens"`
	CompletionTokens int `json:"completion_tokens"`
	TotalTokens      int `json:"total_tokens"`
}

type dsResponse struct {
	Choices []struct {
		Message struct {
			Content string `json:"content"`
		} `json:"message"`
	} `json:"choices"`
	Usage *dsUsage `json:"usage"`
	Error *struct {
		Message string `json:"message"`
	} `json:"error,omitempty"`
}

type dsChunk struct {
	Choices []struct {
		Delta struct {
			Content string `json:"content"`
		} `json:"delta"`
	} `json:"choices"`
	Usage *dsUsage `json:"usage"`
}

// StreamMetrics holds latency/throughput stats captured during a streaming call.
type StreamMetrics struct {
	TTFT      time.Duration // request start → first content token (includes China network trip)
	GenTime   time.Duration // first token → stream end
	Tokens    int           // completion_tokens reported by API
	TokPerSec float64       // Tokens / GenTime
}

// DeepSeekClient makes direct HTTP calls to the DeepSeek REST API.
type DeepSeekClient struct {
	apiKey string
}

// NewDeepSeekClient creates a DeepSeekClient with the given API key.
func NewDeepSeekClient(apiKey string) *DeepSeekClient {
	return &DeepSeekClient{apiKey: apiKey}
}

func noThinking() thinkingConfig { return thinkingConfig{Type: "disabled"} }

func (c *DeepSeekClient) buildMsgs(systemPrompt string, msgs []Message) []dsMsg {
	var out []dsMsg
	if systemPrompt != "" {
		out = append(out, dsMsg{Role: "system", Content: systemPrompt})
	}
	for _, m := range msgs {
		role := m.Role
		if role == "model" {
			role = "assistant"
		}
		out = append(out, dsMsg{Role: role, Content: m.Content})
	}
	return out
}

func (c *DeepSeekClient) post(ctx context.Context, req dsRequest) (*dsResponse, error) {
	b, err := json.Marshal(req)
	if err != nil {
		return nil, err
	}
	hr, err := http.NewRequestWithContext(ctx, "POST", deepseekAPI, bytes.NewReader(b))
	if err != nil {
		return nil, err
	}
	hr.Header.Set("Content-Type", "application/json")
	hr.Header.Set("Authorization", "Bearer "+c.apiKey)

	resp, err := http.DefaultClient.Do(hr)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()

	data, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != 200 {
		return nil, fmt.Errorf("API %d: %s", resp.StatusCode, string(data))
	}
	var dr dsResponse
	if err := json.Unmarshal(data, &dr); err != nil {
		return nil, fmt.Errorf("JSON decode: %w | body: %s", err, string(data))
	}
	if dr.Error != nil {
		return nil, fmt.Errorf("DeepSeek error: %s", dr.Error.Message)
	}
	return &dr, nil
}

// RouteJSON calls DeepSeek with JSON response mode for supervisor routing.
func (c *DeepSeekClient) RouteJSON(ctx context.Context, messages []Message) (Router, error) {
	lastUser := ""
	for i := len(messages) - 1; i >= 0; i-- {
		if messages[i].Role == "user" {
			lastUser = messages[i].Content
			break
		}
	}

	var lines []string
	for _, m := range messages {
		name := m.Name
		if name == "" {
			name = m.Role
		}
		p := m.Content
		if len(p) > 500 {
			p = p[:500] + "..."
		}
		lines = append(lines, fmt.Sprintf("[%s]: %s", strings.ToUpper(name), p))
	}
	prompt := fmt.Sprintf(
		"Yêu cầu gốc: %s\n\nLịch sử:\n%s\n\nQuyết định:",
		lastUser, strings.Join(lines, "\n"),
	)

	resp, err := c.post(ctx, dsRequest{
		Model:          deepseekModel,
		Messages:       []dsMsg{{Role: "system", Content: routerSystem}, {Role: "user", Content: prompt}},
		ResponseFormat: map[string]string{"type": "json_object"},
		Thinking:       noThinking(),
		Temperature:    0,
	})
	if err != nil {
		return Router{}, err
	}

	raw := ""
	if len(resp.Choices) > 0 {
		raw = strings.TrimSpace(resp.Choices[0].Message.Content)
	}
	for _, fence := range []string{"```json", "```"} {
		raw = strings.TrimPrefix(raw, fence)
		raw = strings.TrimSuffix(raw, fence)
	}
	raw = strings.TrimSpace(raw)

	var r Router
	if err := json.Unmarshal([]byte(raw), &r); err != nil {
		return Router{}, fmt.Errorf("router parse: %w | raw: %s", err, raw)
	}
	return r, nil
}

// StreamChat streams a response from DeepSeek, prints tokens in real-time, and
// returns the full text plus performance metrics.
//
// TTFT is measured from the moment the HTTP request is sent, so it includes the
// full network round-trip to DeepSeek's servers.
func (c *DeepSeekClient) StreamChat(ctx context.Context, systemPrompt string, messages []Message) (string, StreamMetrics, error) {
	req := dsRequest{
		Model:         deepseekModel,
		Messages:      c.buildMsgs(systemPrompt, messages),
		Stream:        true,
		Thinking:      noThinking(),
		StreamOptions: &dsStreamOptions{IncludeUsage: true},
		Temperature:   0.7,
	}

	b, err := json.Marshal(req)
	if err != nil {
		return "", StreamMetrics{}, err
	}
	hr, err := http.NewRequestWithContext(ctx, "POST", deepseekAPI, bytes.NewReader(b))
	if err != nil {
		return "", StreamMetrics{}, err
	}
	hr.Header.Set("Content-Type", "application/json")
	hr.Header.Set("Authorization", "Bearer "+c.apiKey)
	hr.Header.Set("Accept", "text/event-stream")

	tRequest := time.Now()
	resp, err := http.DefaultClient.Do(hr)
	if err != nil {
		return "", StreamMetrics{}, err
	}
	defer resp.Body.Close()

	if resp.StatusCode != 200 {
		d, _ := io.ReadAll(resp.Body)
		return "", StreamMetrics{}, fmt.Errorf("stream API %d: %s", resp.StatusCode, string(d))
	}

	var (
		sb          strings.Builder
		tFirstToken time.Time
		tEnd        time.Time
		compTokens  int
	)

	fmt.Println()
	sc := bufio.NewScanner(resp.Body)
	sc.Buffer(make([]byte, 1<<20), 1<<20)
	for sc.Scan() {
		line := sc.Text()
		if !strings.HasPrefix(line, "data: ") {
			continue
		}
		payload := strings.TrimPrefix(line, "data: ")
		if payload == "[DONE]" {
			tEnd = time.Now()
			break
		}
		var chunk dsChunk
		if json.Unmarshal([]byte(payload), &chunk) != nil {
			continue
		}
		if chunk.Usage != nil && chunk.Usage.CompletionTokens > 0 {
			compTokens = chunk.Usage.CompletionTokens
		}
		if len(chunk.Choices) > 0 {
			tok := chunk.Choices[0].Delta.Content
			if tok != "" {
				if tFirstToken.IsZero() {
					tFirstToken = time.Now()
				}
				fmt.Print(tok)
				sb.WriteString(tok)
			}
		}
	}
	fmt.Println()

	if tEnd.IsZero() {
		tEnd = time.Now()
	}

	var m StreamMetrics
	if !tFirstToken.IsZero() {
		m.TTFT = tFirstToken.Sub(tRequest)
		m.GenTime = tEnd.Sub(tFirstToken)
		m.Tokens = compTokens
		if m.GenTime > 0 && compTokens > 0 {
			m.TokPerSec = float64(compTokens) / m.GenTime.Seconds()
		}
	}

	return sb.String(), m, sc.Err()
}

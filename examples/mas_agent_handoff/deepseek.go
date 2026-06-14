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
	dsModel = "deepseek-v4-flash"
	dsAPI   = "https://api.deepseek.com/chat/completions"
)

// ── request types ─────────────────────────────────────────────────────────────

type dsChatMsg struct {
	Role       string       `json:"role"`
	Content    *string      `json:"content"` // pointer: null when tool_calls present
	ToolCalls  []dsToolCall `json:"tool_calls,omitempty"`
	ToolCallID string       `json:"tool_call_id,omitempty"`
}

type dsToolCall struct {
	ID       string `json:"id"`
	Type     string `json:"type"`
	Function struct {
		Name      string `json:"name"`
		Arguments string `json:"arguments"` // JSON-encoded string
	} `json:"function"`
}

type dsFuncDef struct {
	Name        string          `json:"name"`
	Description string          `json:"description"`
	Parameters  json.RawMessage `json:"parameters"`
}

type dsAPITool struct {
	Type     string    `json:"type"` // "function"
	Function dsFuncDef `json:"function"`
}

type dsThinking struct {
	Type string `json:"type"`
}

type dsStreamOptions struct {
	IncludeUsage bool `json:"include_usage"`
}

type dsReq struct {
	Model          string            `json:"model"`
	Messages       []dsChatMsg       `json:"messages"`
	Tools          []dsAPITool       `json:"tools,omitempty"`
	ToolChoice     any               `json:"tool_choice,omitempty"` // "required" | "auto" | nil
	ResponseFormat map[string]string `json:"response_format,omitempty"`
	Temperature    float64           `json:"temperature"`
	MaxTokens      int               `json:"max_tokens,omitempty"`
	Thinking       dsThinking        `json:"thinking"`
	Stream         bool              `json:"stream,omitempty"`
	StreamOptions  *dsStreamOptions  `json:"stream_options,omitempty"`
}

// ── streaming delta types ─────────────────────────────────────────────────────

type dsStreamDelta struct {
	Content   *string           `json:"content"`
	ToolCalls []dsToolCallDelta `json:"tool_calls,omitempty"`
}

type dsToolCallDelta struct {
	Index    int    `json:"index"`
	ID       string `json:"id,omitempty"`
	Type     string `json:"type,omitempty"`
	Function struct {
		Name      string `json:"name,omitempty"`
		Arguments string `json:"arguments,omitempty"`
	} `json:"function"`
}

type dsStreamChunk struct {
	Choices []struct {
		Delta dsStreamDelta `json:"delta"`
	} `json:"choices"`
	Usage *struct {
		PromptTokens     int `json:"prompt_tokens"`
		CompletionTokens int `json:"completion_tokens"`
	} `json:"usage,omitempty"`
}

type dsResp struct {
	Choices []struct {
		Message dsChatMsg `json:"message"`
	} `json:"choices"`
	Usage *struct {
		PromptTokens     int `json:"prompt_tokens"`
		CompletionTokens int `json:"completion_tokens"`
	} `json:"usage"`
	Error *struct{ Message string } `json:"error,omitempty"`
}

// ── client ────────────────────────────────────────────────────────────────────

// DSClient makes HTTP calls to the DeepSeek REST API.
type DSClient struct{ apiKey string }

func NewDSClient(key string) *DSClient { return &DSClient{apiKey: key} }

func strPtr(s string) *string { return &s }

func noThinkDS() dsThinking { return dsThinking{Type: "disabled"} }

func (c *DSClient) post(ctx context.Context, req dsReq) (*dsResp, error) {
	b, err := json.Marshal(req)
	if err != nil {
		return nil, err
	}
	hr, err := http.NewRequestWithContext(ctx, "POST", dsAPI, bytes.NewReader(b))
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
		return nil, fmt.Errorf("DeepSeek %d: %s", resp.StatusCode, string(data))
	}
	var dr dsResp
	if err := json.Unmarshal(data, &dr); err != nil {
		return nil, fmt.Errorf("decode: %w | body: %s", err, string(data))
	}
	if dr.Error != nil {
		return nil, fmt.Errorf("DeepSeek error: %s", dr.Error.Message)
	}
	return &dr, nil
}

// RouteJSON calls DeepSeek with JSON mode and returns a routing decision.
// Returns raw JSON string plus prompt/completion token counts for Langfuse.
func (c *DSClient) RouteJSON(ctx context.Context, systemPrompt, userPrompt string) (string, int, int, error) {
	resp, err := c.post(ctx, dsReq{
		Model: dsModel,
		Messages: []dsChatMsg{
			{Role: "system", Content: strPtr(systemPrompt)},
			{Role: "user", Content: strPtr(userPrompt)},
		},
		ResponseFormat: map[string]string{"type": "json_object"},
		Thinking:       noThinkDS(),
		Temperature:    0,
		MaxTokens:      160,
	})
	if err != nil {
		return "", 0, 0, err
	}
	if len(resp.Choices) == 0 || resp.Choices[0].Message.Content == nil {
		return "", 0, 0, fmt.Errorf("empty response")
	}
	promptTok, completionTok := 0, 0
	if resp.Usage != nil {
		promptTok = resp.Usage.PromptTokens
		completionTok = resp.Usage.CompletionTokens
	}
	return strings.TrimSpace(*resp.Choices[0].Message.Content), promptTok, completionTok, nil
}

// ChatWithTools runs one turn of a ReAct tool-calling loop.
// Returns the assistant message (may contain tool_calls or a final answer).
func (c *DSClient) ChatWithTools(ctx context.Context, messages []dsChatMsg, tools []dsAPITool) (*dsChatMsg, error) {
	resp, err := c.post(ctx, dsReq{
		Model:       dsModel,
		Messages:    messages,
		Tools:       tools,
		Thinking:    noThinkDS(),
		Temperature: 0.3,
	})
	if err != nil {
		return nil, err
	}
	if len(resp.Choices) == 0 {
		return nil, fmt.Errorf("empty response")
	}
	m := resp.Choices[0].Message
	return &m, nil
}

// StreamChatWithTools streams one ReAct turn.
// toolChoice: "required" forces a tool call (use on round 0); nil lets the model decide.
// onToken is called for each text token when the response is a final answer.
// Tool-call responses are accumulated silently and returned in full.
// Returns (message, promptTokens, completionTokens, firstDelta, error).
// firstDelta is set on the very first non-empty delta — text OR tool_call — for TTFT.
func (c *DSClient) StreamChatWithTools(
	ctx context.Context,
	messages []dsChatMsg,
	tools []dsAPITool,
	toolChoice any,
	onToken func(string),
) (*dsChatMsg, int, int, time.Time, error) {
	b, err := json.Marshal(dsReq{
		Model:         dsModel,
		Messages:      messages,
		Tools:         tools,
		ToolChoice:    toolChoice,
		Thinking:      noThinkDS(),
		Temperature:   0.3,
		Stream:        true,
		StreamOptions: &dsStreamOptions{IncludeUsage: true},
	})
	if err != nil {
		return nil, 0, 0, time.Time{}, err
	}
	hr, err := http.NewRequestWithContext(ctx, "POST", dsAPI, bytes.NewReader(b))
	if err != nil {
		return nil, 0, 0, time.Time{}, err
	}
	hr.Header.Set("Content-Type", "application/json")
	hr.Header.Set("Authorization", "Bearer "+c.apiKey)

	resp, err := http.DefaultClient.Do(hr)
	if err != nil {
		return nil, 0, 0, time.Time{}, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != 200 {
		data, _ := io.ReadAll(resp.Body)
		return nil, 0, 0, time.Time{}, fmt.Errorf("DeepSeek stream %d: %s", resp.StatusCode, string(data))
	}

	// deepseek-v4-flash streams preamble text BEFORE tool_call deltas in the same
	// response.  Accumulate both throughout; decide at the end: tool_calls win.
	var contentBuf strings.Builder
	var toolCalls []dsToolCall
	var promptTok, completionTok int
	var firstDelta time.Time

	sc := bufio.NewScanner(resp.Body)
	sc.Buffer(make([]byte, 1<<20), 1<<20)
	for sc.Scan() {
		line := sc.Text()
		if !strings.HasPrefix(line, "data: ") {
			continue
		}
		payload := strings.TrimPrefix(line, "data: ")
		if payload == "[DONE]" {
			break
		}
		var chunk dsStreamChunk
		if json.Unmarshal([]byte(payload), &chunk) != nil {
			continue
		}
		// Usage arrives in a final chunk with empty choices (stream_options.include_usage).
		if chunk.Usage != nil {
			promptTok = chunk.Usage.PromptTokens
			completionTok = chunk.Usage.CompletionTokens
		}
		if len(chunk.Choices) == 0 {
			continue
		}
		delta := chunk.Choices[0].Delta

		// Capture TTFT on first meaningful delta (text or tool_call).
		hasContent := (delta.Content != nil && *delta.Content != "") || len(delta.ToolCalls) > 0
		if firstDelta.IsZero() && hasContent {
			firstDelta = time.Now()
		}

		// Accumulate tool call deltas unconditionally
		for _, tc := range delta.ToolCalls {
			for int(tc.Index) >= len(toolCalls) {
				toolCalls = append(toolCalls, dsToolCall{Type: "function"})
			}
			t := &toolCalls[tc.Index]
			if tc.ID != "" {
				t.ID = tc.ID
			}
			t.Function.Name += tc.Function.Name
			t.Function.Arguments += tc.Function.Arguments
		}

		// Stream text tokens unconditionally (may be preamble before tools, or final answer)
		if delta.Content != nil && *delta.Content != "" {
			tok := *delta.Content
			contentBuf.WriteString(tok)
			if onToken != nil {
				onToken(tok)
			}
		}
	}
	if err := sc.Err(); err != nil {
		return nil, 0, 0, time.Time{}, fmt.Errorf("stream scan: %w", err)
	}

	// Tool calls take priority: return them even when preamble text was also streamed.
	msg := &dsChatMsg{Role: "assistant"}
	if len(toolCalls) > 0 {
		msg.ToolCalls = toolCalls
	} else {
		s := contentBuf.String()
		msg.Content = &s
	}
	return msg, promptTok, completionTok, firstDelta, nil
}

// StreamReply implements SupervisorBackend for DSClient.
func (c *DSClient) StreamReply(ctx context.Context, systemPrompt string, msgs []Message, onToken func(string)) (string, time.Time, error) {
	dsMsgs := []dsChatMsg{{Role: "system", Content: strPtr(systemPrompt)}}
	for _, m := range msgs {
		role := m.Role
		if role == "model" {
			role = "assistant"
		}
		dsMsgs = append(dsMsgs, dsChatMsg{Role: role, Content: strPtr(m.Content)})
	}
	var sb strings.Builder
	resp, _, _, firstDelta, err := c.StreamChatWithTools(ctx, dsMsgs, nil, nil, func(tok string) {
		sb.WriteString(tok)
		onToken(tok)
	})
	if err != nil {
		return "", time.Time{}, err
	}
	if resp.Content != nil && sb.Len() == 0 {
		return *resp.Content, firstDelta, nil
	}
	return sb.String(), firstDelta, nil
}

// buildAPITools converts ToolDef slice to the dsAPITool slice expected by the API.
func buildAPITools(defs []ToolDef) []dsAPITool {
	out := make([]dsAPITool, len(defs))
	for i, d := range defs {
		out[i] = dsAPITool{
			Type: "function",
			Function: dsFuncDef{
				Name:        d.Name,
				Description: d.Description,
				Parameters:  d.Parameters,
			},
		}
	}
	return out
}

// toolsMap returns a name→ToolDef map for fast lookup.
func toolsMap(defs []ToolDef) map[string]ToolDef {
	m := make(map[string]ToolDef, len(defs))
	for _, d := range defs {
		m[d.Name] = d
	}
	return m
}

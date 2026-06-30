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

const dsAPI = "https://api.deepseek.com/chat/completions"

// ── request types ─────────────────────────────────────────────────────────────

type dsChatMsg struct {
	Role       string       `json:"role"                   bson:"role"`
	Content    *string      `json:"content"                bson:"content"`
	ToolCalls  []dsToolCall `json:"tool_calls,omitempty"   bson:"tool_calls,omitempty"`
	ToolCallID string       `json:"tool_call_id,omitempty" bson:"tool_call_id,omitempty"`
}

type dsToolCall struct {
	ID       string `json:"id"   bson:"id"`
	Type     string `json:"type" bson:"type"`
	Function struct {
		Name      string `json:"name"      bson:"name"`
		Arguments string `json:"arguments" bson:"arguments"`
	} `json:"function" bson:"function"`
}

type dsFuncDef struct {
	Name        string          `json:"name"`
	Description string          `json:"description"`
	Parameters  json.RawMessage `json:"parameters"`
}

type dsAPITool struct {
	Type     string    `json:"type"`
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
	ToolChoice     any               `json:"tool_choice,omitempty"`
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

// StreamTiming holds timing metrics captured during one StreamChatWithTools call.
type StreamTiming struct {
	FirstDelta time.Time // absolute wall time of first content or tool-call token (for Langfuse)
	ConnectMS  float64   // HTTP request start → response headers received (ms)
	TTFT_MS    float64   // HTTP request start → first content or tool-call token (ms)
	GenMS      float64   // HTTP request start → stream EOF (ms)
}

// DSClient makes HTTP calls to the DeepSeek REST API.
type DSClient struct {
	apiKey string
	model  string
}

func NewDSClient(key, model string) *DSClient { return &DSClient{apiKey: key, model: model} }

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

// StreamChatWithTools streams one ReAct turn.
// onToken is called for each text token; onToolDelta is called for each tool_call delta.
// Pass nil to disable either callback.
func (c *DSClient) StreamChatWithTools(
	ctx context.Context,
	messages []dsChatMsg,
	tools []dsAPITool,
	toolChoice any,
	onToken func(string),
	onToolDelta func(index int, name, argChunk string),
) (*dsChatMsg, int, int, StreamTiming, error) {
	b, err := json.Marshal(dsReq{
		Model:         c.model,
		Messages:      messages,
		Tools:         tools,
		ToolChoice:    toolChoice,
		Thinking:      noThinkDS(),
		Temperature:   0.3,
		Stream:        true,
		StreamOptions: &dsStreamOptions{IncludeUsage: true},
	})
	if err != nil {
		return nil, 0, 0, StreamTiming{}, err
	}
	hr, err := http.NewRequestWithContext(ctx, "POST", dsAPI, bytes.NewReader(b))
	if err != nil {
		return nil, 0, 0, StreamTiming{}, err
	}
	hr.Header.Set("Content-Type", "application/json")
	hr.Header.Set("Authorization", "Bearer "+c.apiKey)

	t0 := time.Now() // measure from here: includes DNS, TLS, request send
	resp, err := http.DefaultClient.Do(hr)
	if err != nil {
		return nil, 0, 0, StreamTiming{}, err
	}
	connectMS := float64(time.Since(t0).Milliseconds()) // headers received = connection established

	defer resp.Body.Close()
	if resp.StatusCode != 200 {
		data, _ := io.ReadAll(resp.Body)
		return nil, 0, 0, StreamTiming{}, fmt.Errorf("DeepSeek stream %d: %s", resp.StatusCode, string(data))
	}

	var contentBuf strings.Builder
	var toolCalls []dsToolCall
	var promptTok, completionTok int
	var firstDelta time.Time
	var ttftMS float64

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
		if chunk.Usage != nil {
			promptTok = chunk.Usage.PromptTokens
			completionTok = chunk.Usage.CompletionTokens
		}
		if len(chunk.Choices) == 0 {
			continue
		}
		delta := chunk.Choices[0].Delta

		hasContent := (delta.Content != nil && *delta.Content != "") || len(delta.ToolCalls) > 0
		if firstDelta.IsZero() && hasContent {
			firstDelta = time.Now()
			ttftMS = float64(firstDelta.Sub(t0).Milliseconds())
		}

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
			if onToolDelta != nil {
				onToolDelta(tc.Index, tc.Function.Name, tc.Function.Arguments)
			}
		}

		if delta.Content != nil && *delta.Content != "" {
			tok := *delta.Content
			contentBuf.WriteString(tok)
			if onToken != nil {
				onToken(tok)
			}
		}
	}
	if err := sc.Err(); err != nil {
		return nil, 0, 0, StreamTiming{}, fmt.Errorf("stream scan: %w", err)
	}

	timing := StreamTiming{
		FirstDelta: firstDelta,
		ConnectMS:  connectMS,
		TTFT_MS:    ttftMS,
		GenMS:      float64(time.Since(t0).Milliseconds()),
	}

	msg := &dsChatMsg{Role: "assistant"}
	if s := contentBuf.String(); s != "" {
		msg.Content = &s
	}
	if len(toolCalls) > 0 {
		msg.ToolCalls = toolCalls
	}
	return msg, promptTok, completionTok, timing, nil
}

// Summarize calls DeepSeek non-streaming for text summarization.
// onToken (optional) is called for each token so callers can stream progress.
func (c *DSClient) Summarize(ctx context.Context, prompt string, onToken func(string)) (string, error) {
	msgs := []dsChatMsg{{Role: "user", Content: strPtr(prompt)}}
	var sb strings.Builder
	resp, _, _, _, err := c.StreamChatWithTools(ctx, msgs, nil, nil, //nolint:dogsled
		func(tok string) {
			sb.WriteString(tok)
			if onToken != nil {
				onToken(tok)
			}
		},
		nil,
	)
	if err != nil {
		return "", err
	}
	if resp.Content != nil && sb.Len() == 0 {
		return *resp.Content, nil
	}
	return sb.String(), nil
}

// buildAPITools converts ToolDef slice to dsAPITool slice.
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

package main

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
)

const dsEndpoint = "https://api.deepseek.com/chat/completions"

type msg struct {
	Role             string     `json:"role"`
	Content          *string    `json:"content"`
	ReasoningContent *string    `json:"reasoning_content,omitempty"`
	ToolCalls        []toolCall `json:"tool_calls,omitempty"`
	ToolCallID       string     `json:"tool_call_id,omitempty"`
}

type toolCall struct {
	ID       string `json:"id"`
	Type     string `json:"type"`
	Function struct {
		Name      string `json:"name"`
		Arguments string `json:"arguments"`
	} `json:"function"`
}

type toolDef struct {
	Type     string `json:"type"`
	Function struct {
		Name        string          `json:"name"`
		Description string          `json:"description"`
		Parameters  json.RawMessage `json:"parameters"`
	} `json:"function"`
}

type thinkingCfg struct {
	Type            string `json:"type"`
	ReasoningEffort string `json:"reasoning_effort,omitempty"`
}

type apiReq struct {
	Model    string      `json:"model"`
	Messages []msg       `json:"messages"`
	Tools    []toolDef   `json:"tools,omitempty"`
	Thinking thinkingCfg `json:"thinking"`
}

type apiResp struct {
	Choices []struct {
		Message msg `json:"message"`
	} `json:"choices"`
	Error *struct{ Message string } `json:"error,omitempty"`
}

type dsClient struct {
	key      string
	model    string
	thinking bool
}

func newClient(key, model string, thinking bool) *dsClient {
	return &dsClient{key: key, model: model, thinking: thinking}
}

func sp(s string) *string { return &s }

func (c *dsClient) complete(ctx context.Context, messages []msg, tools []toolDef) (*msg, error) {
	t := thinkingCfg{Type: "disabled"}
	if c.thinking {
		t = thinkingCfg{Type: "enabled", ReasoningEffort: "high"}
	}
	body, _ := json.Marshal(apiReq{
		Model:    c.model,
		Messages: messages,
		Tools:    tools,
		Thinking: t,
	})
	req, err := http.NewRequestWithContext(ctx, "POST", dsEndpoint, bytes.NewReader(body))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer "+c.key)

	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	data, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != 200 {
		return nil, fmt.Errorf("API %d: %s", resp.StatusCode, data)
	}
	var dr apiResp
	if err := json.Unmarshal(data, &dr); err != nil {
		return nil, fmt.Errorf("decode: %w", err)
	}
	if dr.Error != nil {
		return nil, fmt.Errorf("API error: %s", dr.Error.Message)
	}
	if len(dr.Choices) == 0 {
		return nil, fmt.Errorf("empty choices")
	}
	m := dr.Choices[0].Message
	return &m, nil
}

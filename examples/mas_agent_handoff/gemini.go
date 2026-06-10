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
)

const (
	gemModel    = "gemini-3.1-flash-lite"
	gemBase     = "https://generativelanguage.googleapis.com/v1beta/models/" + gemModel
	gemSearchMd = gemModel // same model for search grounding
)

// ── Gemini REST API types ─────────────────────────────────────────────────────

type gPart struct {
	Text string `json:"text"`
}
type gContent struct {
	Role  string  `json:"role"`
	Parts []gPart `json:"parts"`
}
type gRequest struct {
	Contents          []gContent       `json:"contents"`
	SystemInstruction *gContent        `json:"systemInstruction,omitempty"`
	GenerationConfig  map[string]any   `json:"generationConfig,omitempty"`
	Tools             []map[string]any `json:"tools,omitempty"`
}

type gCandidate struct {
	Content struct {
		Parts []gPart `json:"parts"`
	} `json:"content"`
	GroundingMetadata *struct {
		GroundingChunks []struct {
			Web *struct {
				URI   string `json:"uri"`
				Title string `json:"title"`
			} `json:"web,omitempty"`
		} `json:"groundingChunks"`
	} `json:"groundingMetadata,omitempty"`
}

type gUsageMeta struct {
	CandidatesTokenCount int `json:"candidatesTokenCount"`
}

type gResponse struct {
	Candidates    []gCandidate `json:"candidates"`
	UsageMetadata *gUsageMeta  `json:"usageMetadata,omitempty"`
	Error         *struct {
		Code    int    `json:"code"`
		Message string `json:"message"`
	} `json:"error,omitempty"`
}

// ── GeminiClient ──────────────────────────────────────────────────────────────

// GeminiClient makes direct HTTP calls to the Gemini REST API.
type GeminiClient struct{ apiKey string }

func NewGeminiClient(key string) *GeminiClient { return &GeminiClient{apiKey: key} }

func (c *GeminiClient) sys(text string) *gContent {
	return &gContent{Role: "user", Parts: []gPart{{Text: text}}}
}

// buildContents converts a Message slice to Gemini contents (strict alternating turns).
func (c *GeminiClient) buildContents(msgs []Message) []gContent {
	var out []gContent
	for _, m := range msgs {
		if m.Role != "user" && m.Role != "model" {
			continue
		}
		if len(out) > 0 && out[len(out)-1].Role == m.Role {
			out[len(out)-1].Parts[0].Text += "\n" + m.Content
		} else {
			out = append(out, gContent{Role: m.Role, Parts: []gPart{{Text: m.Content}}})
		}
	}
	return out
}

func (c *GeminiClient) callAPI(ctx context.Context, req gRequest) (*gResponse, error) {
	b, err := json.Marshal(req)
	if err != nil {
		return nil, err
	}
	url := gemBase + ":generateContent?key=" + c.apiKey
	hr, err := http.NewRequestWithContext(ctx, "POST", url, bytes.NewReader(b))
	if err != nil {
		return nil, err
	}
	hr.Header.Set("Content-Type", "application/json")
	resp, err := http.DefaultClient.Do(hr)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	data, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != 200 {
		return nil, fmt.Errorf("Gemini %d: %s", resp.StatusCode, string(data))
	}
	var gr gResponse
	if err := json.Unmarshal(data, &gr); err != nil {
		return nil, fmt.Errorf("decode: %w", err)
	}
	if gr.Error != nil {
		return nil, fmt.Errorf("Gemini error %d: %s", gr.Error.Code, gr.Error.Message)
	}
	return &gr, nil
}

// RouteJSON calls Gemini with JSON response mode for supervisor routing.
func (c *GeminiClient) RouteJSON(ctx context.Context, systemPrompt, userPrompt string) (string, error) {
	resp, err := c.callAPI(ctx, gRequest{
		Contents:          []gContent{{Role: "user", Parts: []gPart{{Text: userPrompt}}}},
		SystemInstruction: c.sys(systemPrompt),
		GenerationConfig:  map[string]any{"responseMimeType": "application/json"},
	})
	if err != nil {
		return "", err
	}
	if len(resp.Candidates) == 0 || len(resp.Candidates[0].Content.Parts) == 0 {
		return "", fmt.Errorf("empty Gemini response")
	}
	raw := strings.TrimSpace(resp.Candidates[0].Content.Parts[0].Text)
	for _, fence := range []string{"```json", "```"} {
		raw = strings.TrimPrefix(raw, fence)
		raw = strings.TrimSuffix(raw, fence)
	}
	return strings.TrimSpace(raw), nil
}

// StreamChat streams a chat response, forwarding each token to the provided channel.
func (c *GeminiClient) StreamChat(ctx context.Context, systemPrompt string, msgs []Message, tokCh chan<- string) error {
	req := gRequest{
		Contents:          c.buildContents(msgs),
		SystemInstruction: c.sys(systemPrompt),
	}
	b, err := json.Marshal(req)
	if err != nil {
		return err
	}
	url := gemBase + ":streamGenerateContent?alt=sse&key=" + c.apiKey
	hr, err := http.NewRequestWithContext(ctx, "POST", url, bytes.NewReader(b))
	if err != nil {
		return err
	}
	hr.Header.Set("Content-Type", "application/json")
	resp, err := http.DefaultClient.Do(hr)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode != 200 {
		d, _ := io.ReadAll(resp.Body)
		return fmt.Errorf("Gemini stream %d: %s", resp.StatusCode, string(d))
	}

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
		var gr gResponse
		if json.Unmarshal([]byte(payload), &gr) != nil {
			continue
		}
		if len(gr.Candidates) > 0 && len(gr.Candidates[0].Content.Parts) > 0 {
			if tok := gr.Candidates[0].Content.Parts[0].Text; tok != "" {
				tokCh <- tok
			}
		}
	}
	return sc.Err()
}

// StreamReply implements SupervisorBackend for GeminiClient.
func (c *GeminiClient) StreamReply(ctx context.Context, systemPrompt string, msgs []Message, onToken func(string)) (string, error) {
	tokCh := make(chan string, 256)
	errc := make(chan error, 1)
	go func() {
		errc <- c.StreamChat(ctx, systemPrompt, msgs, tokCh)
		close(tokCh)
	}()
	var sb strings.Builder
	for tok := range tokCh {
		sb.WriteString(tok)
		onToken(tok)
	}
	return sb.String(), <-errc
}

// WebSearch calls Gemini with Google Search grounding (non-streaming, to preserve citations).
type Citation struct {
	Title string
	URL   string
}

func (c *GeminiClient) WebSearch(ctx context.Context, msgs []Message) (string, []Citation, error) {
	resp, err := c.callAPI(ctx, gRequest{
		Contents: c.buildContents(msgs),
		SystemInstruction: c.sys(
			"You are a Web Search Expert. Use Google Search to find the latest information. " +
				"Provide a comprehensive answer in Vietnamese with source citations.",
		),
		Tools: []map[string]any{{"googleSearch": map[string]any{}}},
	})
	if err != nil {
		return "", nil, err
	}
	text := ""
	if len(resp.Candidates) > 0 && len(resp.Candidates[0].Content.Parts) > 0 {
		text = resp.Candidates[0].Content.Parts[0].Text
	}
	var citations []Citation
	if len(resp.Candidates) > 0 && resp.Candidates[0].GroundingMetadata != nil {
		seen := map[string]bool{}
		for _, ch := range resp.Candidates[0].GroundingMetadata.GroundingChunks {
			if ch.Web != nil && !seen[ch.Web.URI] {
				seen[ch.Web.URI] = true
				citations = append(citations, Citation{Title: ch.Web.Title, URL: ch.Web.URI})
			}
		}
	}
	return text, citations, nil
}

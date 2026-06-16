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
	gemModel    = "gemini-3.1-flash-lite"
	gemBase     = "https://generativelanguage.googleapis.com/v1beta/models/" + gemModel
	gemSearchMd = gemModel // same model for search grounding
)

// ── Gemini REST API types ─────────────────────────────────────────────────────

type gPart struct {
	Text       string       `json:"text,omitempty"`
	InlineData *gInlineData `json:"inlineData,omitempty"`
}

type gInlineData struct {
	MimeType string `json:"mimeType"`
	Data     string `json:"data"` // base64
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
	PromptTokenCount     int `json:"promptTokenCount"`
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
// Returns raw JSON string plus prompt/completion token counts for Langfuse.
func (c *GeminiClient) RouteJSON(ctx context.Context, systemPrompt, userPrompt string) (string, int, int, error) {
	resp, err := c.callAPI(ctx, gRequest{
		Contents:          []gContent{{Role: "user", Parts: []gPart{{Text: userPrompt}}}},
		SystemInstruction: c.sys(systemPrompt),
		GenerationConfig:  map[string]any{"responseMimeType": "application/json"},
	})
	if err != nil {
		return "", 0, 0, err
	}
	if len(resp.Candidates) == 0 || len(resp.Candidates[0].Content.Parts) == 0 {
		return "", 0, 0, fmt.Errorf("empty Gemini response")
	}
	raw := strings.TrimSpace(resp.Candidates[0].Content.Parts[0].Text)
	for _, fence := range []string{"```json", "```"} {
		raw = strings.TrimPrefix(raw, fence)
		raw = strings.TrimSuffix(raw, fence)
	}
	promptTok, completionTok := 0, 0
	if resp.UsageMetadata != nil {
		promptTok = resp.UsageMetadata.PromptTokenCount
		completionTok = resp.UsageMetadata.CandidatesTokenCount
	}
	return strings.TrimSpace(raw), promptTok, completionTok, nil
}

// StreamChat streams a chat response, forwarding each token to the provided channel.
// Returns firstDelta — the time of the first non-empty text chunk — for TTFT tracking.
func (c *GeminiClient) StreamChat(ctx context.Context, systemPrompt string, msgs []Message, tokCh chan<- string) (time.Time, error) {
	req := gRequest{
		Contents:          c.buildContents(msgs),
		SystemInstruction: c.sys(systemPrompt),
	}
	b, err := json.Marshal(req)
	if err != nil {
		return time.Time{}, err
	}
	url := gemBase + ":streamGenerateContent?alt=sse&key=" + c.apiKey
	hr, err := http.NewRequestWithContext(ctx, "POST", url, bytes.NewReader(b))
	if err != nil {
		return time.Time{}, err
	}
	hr.Header.Set("Content-Type", "application/json")
	resp, err := http.DefaultClient.Do(hr)
	if err != nil {
		return time.Time{}, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != 200 {
		d, _ := io.ReadAll(resp.Body)
		return time.Time{}, fmt.Errorf("Gemini stream %d: %s", resp.StatusCode, string(d))
	}

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
		var gr gResponse
		if json.Unmarshal([]byte(payload), &gr) != nil {
			continue
		}
		if len(gr.Candidates) > 0 && len(gr.Candidates[0].Content.Parts) > 0 {
			if tok := gr.Candidates[0].Content.Parts[0].Text; tok != "" {
				if firstDelta.IsZero() {
					firstDelta = time.Now()
				}
				tokCh <- tok
			}
		}
	}
	return firstDelta, sc.Err()
}

// StreamReply implements SupervisorBackend for GeminiClient.
func (c *GeminiClient) StreamReply(ctx context.Context, systemPrompt string, msgs []Message, onToken func(string)) (string, time.Time, error) {
	tokCh := make(chan string, 256)
	type result struct {
		firstDelta time.Time
		err        error
	}
	resCh := make(chan result, 1)
	go func() {
		fd, err := c.StreamChat(ctx, systemPrompt, msgs, tokCh)
		close(tokCh)
		resCh <- result{fd, err}
	}()
	var sb strings.Builder
	for tok := range tokCh {
		sb.WriteString(tok)
		onToken(tok)
	}
	r := <-resCh
	return sb.String(), r.firstDelta, r.err
}

// StreamChatWithImage streams a Gemini VLM response. The image is prepended to the
// last user content part; history is carried as normal text-only messages.
// Returns (fullText, promptTokens, completionTokens, firstDelta, error).
// firstDelta is the time of the first non-empty text chunk — used for TTFT.
func (c *GeminiClient) StreamChatWithImage(
	ctx context.Context,
	systemPrompt string,
	msgs []Message,
	imageB64, imageMime string,
	onToken func(string),
) (string, int, int, time.Time, error) {
	contents := c.buildContents(msgs)
	// Prepend the image part to the last user turn.
	for i := len(contents) - 1; i >= 0; i-- {
		if contents[i].Role == "user" {
			contents[i].Parts = append([]gPart{
				{InlineData: &gInlineData{MimeType: imageMime, Data: imageB64}},
			}, contents[i].Parts...)
			break
		}
	}

	req := gRequest{
		Contents:          contents,
		SystemInstruction: c.sys(systemPrompt),
	}
	b, err := json.Marshal(req)
	if err != nil {
		return "", 0, 0, time.Time{}, err
	}
	url := gemBase + ":streamGenerateContent?alt=sse&key=" + c.apiKey
	hr, err := http.NewRequestWithContext(ctx, "POST", url, bytes.NewReader(b))
	if err != nil {
		return "", 0, 0, time.Time{}, err
	}
	hr.Header.Set("Content-Type", "application/json")
	resp, err := http.DefaultClient.Do(hr)
	if err != nil {
		return "", 0, 0, time.Time{}, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != 200 {
		d, _ := io.ReadAll(resp.Body)
		return "", 0, 0, time.Time{}, fmt.Errorf("Gemini VLM stream %d: %s", resp.StatusCode, string(d))
	}

	var sb strings.Builder
	var promptTok, completionTok int
	var firstDelta time.Time
	sc := bufio.NewScanner(resp.Body)
	sc.Buffer(make([]byte, 4<<20), 4<<20) // 4 MB — VLM responses can be longer
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
		if gr.UsageMetadata != nil {
			promptTok = gr.UsageMetadata.PromptTokenCount
			completionTok = gr.UsageMetadata.CandidatesTokenCount
		}
		if len(gr.Candidates) > 0 && len(gr.Candidates[0].Content.Parts) > 0 {
			if tok := gr.Candidates[0].Content.Parts[0].Text; tok != "" {
				if firstDelta.IsZero() {
					firstDelta = time.Now()
				}
				sb.WriteString(tok)
				if onToken != nil {
					onToken(tok)
				}
			}
		}
	}
	if err := sc.Err(); err != nil {
		return "", 0, 0, time.Time{}, fmt.Errorf("VLM stream scan: %w", err)
	}
	return sb.String(), promptTok, completionTok, firstDelta, nil
}

const webSearchSystemPrompt = "You are a Web Search Expert. Use Google Search to find the latest information. " +
	"Provide a comprehensive answer in Vietnamese with source citations."

// WebSearch calls Gemini with Google Search grounding (non-streaming, to preserve citations).
// Returns text, citations, promptTokens, completionTokens, error.
type Citation struct {
	Title string
	URL   string
}

func (c *GeminiClient) WebSearch(ctx context.Context, msgs []Message) (string, []Citation, int, int, error) {
	resp, err := c.callAPI(ctx, gRequest{
		Contents:          c.buildContents(msgs),
		SystemInstruction: c.sys(webSearchSystemPrompt),
		Tools:             []map[string]any{{"googleSearch": map[string]any{}}},
	})
	if err != nil {
		return "", nil, 0, 0, err
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
	promptTok, completionTok := 0, 0
	if resp.UsageMetadata != nil {
		promptTok = resp.UsageMetadata.PromptTokenCount
		completionTok = resp.UsageMetadata.CandidatesTokenCount
	}
	return text, citations, promptTok, completionTok, nil
}

// StreamWebSearch streams a Gemini response with Google Search grounding.
// Tokens are forwarded to onToken as they arrive; grounding citations are returned on completion.
func (c *GeminiClient) StreamWebSearch(ctx context.Context, msgs []Message, onToken func(string)) (string, []Citation, int, int, error) {
	req := gRequest{
		Contents:          c.buildContents(msgs),
		SystemInstruction: c.sys(webSearchSystemPrompt),
		Tools:             []map[string]any{{"googleSearch": map[string]any{}}},
	}
	b, err := json.Marshal(req)
	if err != nil {
		return "", nil, 0, 0, err
	}
	url := gemBase + ":streamGenerateContent?alt=sse&key=" + c.apiKey
	hr, err := http.NewRequestWithContext(ctx, "POST", url, bytes.NewReader(b))
	if err != nil {
		return "", nil, 0, 0, err
	}
	hr.Header.Set("Content-Type", "application/json")
	resp, err := http.DefaultClient.Do(hr)
	if err != nil {
		return "", nil, 0, 0, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != 200 {
		d, _ := io.ReadAll(resp.Body)
		return "", nil, 0, 0, fmt.Errorf("Gemini search stream %d: %s", resp.StatusCode, string(d))
	}

	var sb strings.Builder
	var citations []Citation
	seenCitations := map[string]bool{}
	var promptTok, completionTok int
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
		if gr.UsageMetadata != nil {
			promptTok = gr.UsageMetadata.PromptTokenCount
			completionTok = gr.UsageMetadata.CandidatesTokenCount
		}
		if len(gr.Candidates) == 0 {
			continue
		}
		cand := gr.Candidates[0]
		if len(cand.Content.Parts) > 0 {
			if tok := cand.Content.Parts[0].Text; tok != "" {
				sb.WriteString(tok)
				if onToken != nil {
					onToken(tok)
				}
			}
		}
		if cand.GroundingMetadata != nil {
			for _, ch := range cand.GroundingMetadata.GroundingChunks {
				if ch.Web != nil && !seenCitations[ch.Web.URI] {
					seenCitations[ch.Web.URI] = true
					citations = append(citations, Citation{Title: ch.Web.Title, URL: ch.Web.URI})
				}
			}
		}
	}
	if err := sc.Err(); err != nil {
		return "", nil, 0, 0, fmt.Errorf("web search stream scan: %w", err)
	}
	return sb.String(), citations, promptTok, completionTok, nil
}

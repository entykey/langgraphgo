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
	geminiModel = "gemini-3.1-flash-lite"
	geminiBase  = "https://generativelanguage.googleapis.com/v1beta/models/" + geminiModel
)

// ── internal Gemini REST API types ───────────────────────────────────────────

type gPart struct {
	Text string `json:"text"`
}

type gContent struct {
	Role  string  `json:"role"`
	Parts []gPart `json:"parts"`
}

type gRequest struct {
	Contents          []gContent               `json:"contents"`
	SystemInstruction *gContent                `json:"systemInstruction,omitempty"`
	GenerationConfig  map[string]interface{}   `json:"generationConfig,omitempty"`
	Tools             []map[string]interface{} `json:"tools,omitempty"`
}

type gCandidate struct {
	Content struct {
		Parts []gPart `json:"parts"`
	} `json:"content"`
	GroundingMetadata *gGroundingMeta `json:"groundingMetadata,omitempty"`
}

type gGroundingMeta struct {
	GroundingChunks []struct {
		Web *struct {
			URI   string `json:"uri"`
			Title string `json:"title"`
		} `json:"web,omitempty"`
	} `json:"groundingChunks"`
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

// StreamMetrics holds latency/throughput stats captured during a streaming call.
type StreamMetrics struct {
	TTFT      time.Duration // request start → first content token (includes network)
	GenTime   time.Duration // first token → stream end
	Tokens    int           // candidatesTokenCount from usageMetadata
	TokPerSec float64       // Tokens / GenTime
}

// ── GeminiClient ─────────────────────────────────────────────────────────────

// GeminiClient makes direct HTTP calls to the Gemini REST API.
// It exposes three high-level methods used by the graph node functions.
type GeminiClient struct {
	apiKey string
}

// NewGeminiClient creates a new GeminiClient with the given API key.
func NewGeminiClient(apiKey string) *GeminiClient {
	return &GeminiClient{apiKey: apiKey}
}

// sysContent wraps a system prompt string into the Gemini systemInstruction format.
func (c *GeminiClient) sysContent(text string) *gContent {
	return &gContent{Role: "user", Parts: []gPart{{Text: text}}}
}

// buildContents converts our Message slice into Gemini API contents.
// The Gemini API requires strictly alternating user/model turns, so consecutive
// messages with the same role are merged into one.
func (c *GeminiClient) buildContents(msgs []Message) []gContent {
	var out []gContent
	for _, m := range msgs {
		if m.Role != "user" && m.Role != "model" {
			continue
		}
		if len(out) > 0 && out[len(out)-1].Role == m.Role {
			// merge into previous turn
			out[len(out)-1].Parts[0].Text += "\n" + m.Content
		} else {
			out = append(out, gContent{Role: m.Role, Parts: []gPart{{Text: m.Content}}})
		}
	}
	return out
}

// callAPI sends a non-streaming POST to :generateContent and returns the parsed response.
func (c *GeminiClient) callAPI(ctx context.Context, req gRequest) (*gResponse, error) {
	b, err := json.Marshal(req)
	if err != nil {
		return nil, err
	}
	url := geminiBase + ":generateContent?key=" + c.apiKey
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
		return nil, fmt.Errorf("API %d: %s", resp.StatusCode, string(data))
	}
	var gr gResponse
	if err := json.Unmarshal(data, &gr); err != nil {
		return nil, fmt.Errorf("JSON decode: %w | body: %s", err, string(data))
	}
	if gr.Error != nil {
		return nil, fmt.Errorf("Gemini error %d: %s", gr.Error.Code, gr.Error.Message)
	}
	return &gr, nil
}

// ── Public high-level methods ─────────────────────────────────────────────────

// RouteJSON calls the Gemini API with JSON response mode and parses the
// supervisor's routing decision {"reasoning":"...","next":"..."}.
func (c *GeminiClient) RouteJSON(ctx context.Context, messages []Message) (Router, error) {
	// Build a routing prompt that shows the full message history and the original request.
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
		lastUser,
		strings.Join(lines, "\n"),
	)

	resp, err := c.callAPI(ctx, gRequest{
		Contents:          []gContent{{Role: "user", Parts: []gPart{{Text: prompt}}}},
		SystemInstruction: c.sysContent(routerSystem),
		GenerationConfig:  map[string]interface{}{"responseMimeType": "application/json"},
	})
	if err != nil {
		return Router{}, err
	}

	raw := ""
	if len(resp.Candidates) > 0 && len(resp.Candidates[0].Content.Parts) > 0 {
		raw = resp.Candidates[0].Content.Parts[0].Text
	}
	raw = strings.TrimSpace(raw)
	// strip markdown fences in case the model wraps them despite JSON mode
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

// StreamChat calls Gemini with streaming SSE, prints each token to stdout in
// real-time, and returns the assembled text plus performance metrics.
//
// TTFT is measured from the moment the HTTP request is sent, so it includes
// the full network round-trip to Gemini's servers.
func (c *GeminiClient) StreamChat(ctx context.Context, systemPrompt string, messages []Message) (string, StreamMetrics, error) {
	req := gRequest{
		Contents:          c.buildContents(messages),
		SystemInstruction: c.sysContent(systemPrompt),
	}
	b, err := json.Marshal(req)
	if err != nil {
		return "", StreamMetrics{}, err
	}
	url := geminiBase + ":streamGenerateContent?alt=sse&key=" + c.apiKey
	hr, err := http.NewRequestWithContext(ctx, "POST", url, bytes.NewReader(b))
	if err != nil {
		return "", StreamMetrics{}, err
	}
	hr.Header.Set("Content-Type", "application/json")

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
		var gr gResponse
		if json.Unmarshal([]byte(payload), &gr) != nil {
			continue
		}
		if gr.UsageMetadata != nil && gr.UsageMetadata.CandidatesTokenCount > 0 {
			compTokens = gr.UsageMetadata.CandidatesTokenCount
		}
		if len(gr.Candidates) > 0 && len(gr.Candidates[0].Content.Parts) > 0 {
			if tok := gr.Candidates[0].Content.Parts[0].Text; tok != "" {
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

// WebSearch calls Gemini with the googleSearch grounding tool (non-streaming).
// Returns the full response text and any grounding citations.
func (c *GeminiClient) WebSearch(ctx context.Context, messages []Message) (string, []Citation, error) {
	resp, err := c.callAPI(ctx, gRequest{
		Contents: c.buildContents(messages),
		SystemInstruction: c.sysContent(
			"Bạn là Web Search Expert. Dùng Google Search tìm thông tin chính xác, mới nhất. " +
				"Trả lời đầy đủ, trích dẫn nguồn. Tiếng Việt.",
		),
		Tools: []map[string]interface{}{{"googleSearch": map[string]interface{}{}}},
	})
	if err != nil {
		return "", nil, err
	}

	fullText := ""
	if len(resp.Candidates) > 0 && len(resp.Candidates[0].Content.Parts) > 0 {
		fullText = resp.Candidates[0].Content.Parts[0].Text
	}

	var citations []Citation
	if len(resp.Candidates) > 0 && resp.Candidates[0].GroundingMetadata != nil {
		seen := make(map[string]bool)
		for _, chunk := range resp.Candidates[0].GroundingMetadata.GroundingChunks {
			if chunk.Web != nil && !seen[chunk.Web.URI] {
				seen[chunk.Web.URI] = true
				citations = append(citations, Citation{
					Title: chunk.Web.Title,
					URL:   chunk.Web.URI,
				})
			}
		}
	}

	return fullText, citations, nil
}

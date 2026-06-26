package main

import (
	"bufio"
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"
)

const gemAPIBase = "https://generativelanguage.googleapis.com/v1beta/models/"

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
type GeminiClient struct {
	apiKey string
	model  string // e.g. "gemini-3.1-flash-lite"
}

func NewGeminiClient(key, model string) *GeminiClient {
	return &GeminiClient{apiKey: key, model: model}
}

func (c *GeminiClient) baseURL() string { return gemAPIBase + c.model }

func (c *GeminiClient) sys(text string) *gContent {
	return &gContent{Role: "user", Parts: []gPart{{Text: text}}}
}

func (c *GeminiClient) callAPI(ctx context.Context, req gRequest) (*gResponse, error) {
	b, err := json.Marshal(req)
	if err != nil {
		return nil, err
	}
	url := c.baseURL() + ":generateContent?key=" + c.apiKey
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

// StreamWebSearch streams a Gemini response with Google Search grounding.
// Returns (text, citations, promptTokens, completionTokens, error).
type Citation struct {
	Title string
	URL   string
}

func (c *GeminiClient) StreamWebSearch(ctx context.Context, query string, onToken func(string)) (string, []Citation, int, int, error) {
	req := gRequest{
		Contents: []gContent{{
			Role:  "user",
			Parts: []gPart{{Text: query}},
		}},
		SystemInstruction: c.sys("You are a Web Search Expert. Use Google Search to find the latest information. " +
			"Provide a comprehensive answer in Vietnamese with source citations."),
		Tools: []map[string]any{{"googleSearch": map[string]any{}}},
	}
	b, err := json.Marshal(req)
	if err != nil {
		return "", nil, 0, 0, err
	}
	url := c.baseURL() + ":streamGenerateContent?alt=sse&key=" + c.apiKey
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

// AnalyzeImage sends image bytes to Gemini vision and returns a text description.
// imageB64 is a base64-encoded image; imageMime is its MIME type.
// If urlOrData starts with "http" the image is fetched first.
// If it's a UUID it is resolved from the server-side image cache.
func (c *GeminiClient) AnalyzeImage(ctx context.Context, imageB64, imageMime string) (string, error) {
	resp, err := c.callAPI(ctx, gRequest{
		Contents: []gContent{{
			Role: "user",
			Parts: []gPart{
				{InlineData: &gInlineData{MimeType: imageMime, Data: imageB64}},
				{Text: "Mô tả chi tiết nội dung ảnh. Nếu có văn bản, hãy đọc đầy đủ. Trả lời bằng tiếng Việt."},
			},
		}},
	})
	if err != nil {
		return "", err
	}
	if len(resp.Candidates) == 0 || len(resp.Candidates[0].Content.Parts) == 0 {
		return "", fmt.Errorf("empty Gemini VLM response")
	}
	return resp.Candidates[0].Content.Parts[0].Text, nil
}

// FetchAndAnalyzeImage resolves a URL, data URI, or server image_id, then calls AnalyzeImage.
func (c *GeminiClient) FetchAndAnalyzeImage(ctx context.Context, urlOrData string) (string, error) {
	urlOrData = strings.TrimSpace(urlOrData)
	urlOrData = strings.Trim(urlOrData, `"'`)

	// UUID → server-side image cache
	if isUUID(urlOrData) {
		b64, mime, ok := getImageForLLM(urlOrData)
		if !ok {
			return "", fmt.Errorf("image ID '%s' not found — may have expired", urlOrData)
		}
		return c.AnalyzeImage(ctx, b64, mime)
	}

	var imgBytes []byte
	var mime string

	switch {
	case strings.HasPrefix(urlOrData, "http://") || strings.HasPrefix(urlOrData, "https://"):
		resp, err := http.Get(urlOrData) //nolint:noctx
		if err != nil {
			return "", fmt.Errorf("fetch image: %w", err)
		}
		defer resp.Body.Close()
		imgBytes, _ = io.ReadAll(resp.Body)
		mime = resp.Header.Get("Content-Type")
		if mime == "" {
			mime = "image/jpeg"
		}
		mime = strings.SplitN(mime, ";", 2)[0]

	case strings.HasPrefix(urlOrData, "data:"):
		parts := strings.SplitN(urlOrData, ",", 2)
		if len(parts) != 2 {
			return "", fmt.Errorf("malformed data URI")
		}
		header := strings.Split(parts[0], ";")[0]
		mime = strings.TrimPrefix(header, "data:")
		var err error
		imgBytes, err = base64.StdEncoding.DecodeString(parts[1])
		if err != nil {
			return "", fmt.Errorf("base64 decode: %w", err)
		}

	default:
		var err error
		imgBytes, err = base64.StdEncoding.DecodeString(urlOrData)
		if err != nil {
			return "", fmt.Errorf("raw base64 decode: %w", err)
		}
		mime = "image/jpeg"
	}

	b64 := base64.StdEncoding.EncodeToString(imgBytes)
	return c.AnalyzeImage(ctx, b64, mime)
}

// isUUID returns true if s looks like a UUID (8-4-4-4-12 hex).
func isUUID(s string) bool {
	if len(s) != 36 {
		return false
	}
	for i, c := range s {
		if i == 8 || i == 13 || i == 18 || i == 23 {
			if c != '-' {
				return false
			}
		} else if !((c >= '0' && c <= '9') || (c >= 'a' && c <= 'f') || (c >= 'A' && c <= 'F')) {
			return false
		}
	}
	return true
}

// detectMimeByExt returns a MIME type based on filename extension.
func detectMimeByExt(filename string) string {
	dot := strings.LastIndex(filename, ".")
	if dot < 0 {
		return "application/octet-stream"
	}
	switch strings.ToLower(filename[dot+1:]) {
	case "jpg", "jpeg":
		return "image/jpeg"
	case "png":
		return "image/png"
	case "gif":
		return "image/gif"
	case "webp":
		return "image/webp"
	case "pdf":
		return "application/pdf"
	case "xlsx":
		return "application/vnd.openxmlformats-officedocument.spreadsheetml.sheet"
	case "docx":
		return "application/vnd.openxmlformats-officedocument.wordprocessingml.document"
	case "csv":
		return "text/csv"
	case "txt":
		return "text/plain"
	default:
		return "application/octet-stream"
	}
}

// StreamChatWithImage streams a Gemini VLM response. Used by mas_agent_handoff compat.
func (c *GeminiClient) StreamChatWithImage(
	ctx context.Context,
	_ string,
	msgs []messageJSON,
	imageB64, imageMime string,
	onToken func(string),
) (string, int, int, time.Time, error) {
	// Build user message with image + text
	userText := ""
	for _, m := range msgs {
		if m.Role == "user" {
			userText = m.Content
		}
	}
	resp, err := c.callAPI(ctx, gRequest{
		Contents: []gContent{{
			Role: "user",
			Parts: []gPart{
				{InlineData: &gInlineData{MimeType: imageMime, Data: imageB64}},
				{Text: userText},
			},
		}},
	})
	if err != nil {
		return "", 0, 0, time.Time{}, err
	}
	text := ""
	if len(resp.Candidates) > 0 && len(resp.Candidates[0].Content.Parts) > 0 {
		text = resp.Candidates[0].Content.Parts[0].Text
	}
	if onToken != nil {
		onToken(text)
	}
	promptTok, completionTok := 0, 0
	if resp.UsageMetadata != nil {
		promptTok = resp.UsageMetadata.PromptTokenCount
		completionTok = resp.UsageMetadata.CandidatesTokenCount
	}
	return text, promptTok, completionTok, time.Now(), nil
}

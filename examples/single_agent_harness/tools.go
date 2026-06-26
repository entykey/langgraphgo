package main

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync"
)

// ToolDef describes a callable function tool for DeepSeek function calling.
type ToolDef struct {
	Name        string
	Description string
	Parameters  json.RawMessage
	Fn          func(args map[string]any) string
}

// ── Skill system ──────────────────────────────────────────────────────────────

// skillsDir is resolved at startup relative to the executable.
var skillsDir string

func initSkillsDir() {
	exe, err := os.Executable()
	if err != nil {
		skillsDir = "skills"
		return
	}
	// When run via `go run ./single_agent_harness/`, exe is in a temp dir.
	// Walk up to find the skills/ directory alongside the source.
	candidates := []string{
		filepath.Join(filepath.Dir(exe), "skills"),
		"skills",
		"examples/single_agent_harness/skills",
		"single_agent_harness/skills",
	}
	for _, c := range candidates {
		if info, err := os.Stat(c); err == nil && info.IsDir() {
			skillsDir = c
			return
		}
	}
	skillsDir = "skills"
}

// skillToolsMap maps skill name → domain tools activated when that skill is loaded.
var skillToolsMap = map[string][]ToolDef{
	"jsonplaceholder": JSONPlaceholderTools,
}

// ── Core tool factory ─────────────────────────────────────────────────────────

// makeCoreTools returns the tools that are always available (load_skill, web_search,
// read_image, write_file, list_workspace).
// activeSkills is a live reference to the session's skill set — mutations by load_skill persist.
// toolMapRef allows load_skill to inject new domain tools for the current turn.
func makeCoreTools(
	ctx context.Context,
	sessionID string,
	activeSkills map[string]bool,
	toolMapRef *map[string]ToolDef,
	eventCh chan<- SSEEvent,
) []ToolDef {
	loadSkill := ToolDef{
		Name:        "load_skill",
		Description: "Read detailed domain documentation before handling a domain-specific request. After loading, the domain's tools become available in subsequent rounds.",
		Parameters:  json.RawMessage(`{"type":"object","properties":{"skill_name":{"type":"string","description":"Name of the skill to load"}},"required":["skill_name"]}`),
		Fn: func(args map[string]any) string {
			name, _ := args["skill_name"].(string)
			if name == "" {
				return "Error: skill_name is required."
			}
			if activeSkills[name] {
				return fmt.Sprintf("Skill '%s' đã active trong session — các tool của nó đã sẵn sàng.", name)
			}
			path := filepath.Join(skillsDir, name+".md")
			data, err := os.ReadFile(path)
			if err != nil {
				available := listSkills()
				return fmt.Sprintf("Skill '%s' không tồn tại. Có sẵn: %s", name, strings.Join(available, ", "))
			}
			activeSkills[name] = true
			// Inject domain tools into the live toolMap so they're available next round.
			if tm := *toolMapRef; tm != nil {
				for _, t := range skillToolsMap[name] {
					tm[t.Name] = t
				}
			}
			fmt.Printf("[skill] loaded '%s' (%d chars) for session %s\n", name, len(data), sessionID[:8])
			return fmt.Sprintf("[SKILL LOADED: %s]\n\n%s", name, string(data))
		},
	}

	webSearch := ToolDef{
		Name:        "web_search",
		Description: "Search the web for current information, news, prices, or real-world facts.",
		Parameters:  json.RawMessage(`{"type":"object","properties":{"query":{"type":"string","description":"Search query in Vietnamese or English"}},"required":["query"]}`),
		Fn: func(args map[string]any) string {
			query, _ := args["query"].(string)
			if query == "" {
				return "Error: missing query"
			}
			fmt.Printf("[web_search] %s\n", query)
			text, citations, promptTok, completionTok, err := geminiClient.StreamWebSearch(ctx, query, func(tok string) {
				emit(eventCh, "tool_stream", map[string]string{"name": "web_search", "text": tok})
			})
			if err != nil {
				return "Error: " + err.Error()
			}
			emit(eventCh, "usage", map[string]any{
				"agent": "web_search", "prompt_tok": promptTok, "completion_tok": completionTok,
			})
			if len(citations) > 0 {
				emit(eventCh, "citations", map[string]any{"count": len(citations)})
				var sb strings.Builder
				sb.WriteString(text)
				sb.WriteString("\n\n---\n**Nguồn tham khảo:**\n")
				for i, c := range citations {
					fmt.Fprintf(&sb, "%d. [%s](%s)\n", i+1, c.Title, c.URL)
				}
				return sb.String()
			}
			return text
		},
	}

	readImage := ToolDef{
		Name:        "read_image",
		Description: "Analyze an image using vision AI. Accepts a URL, base64 data URI, or a server-side image_id (UUID).",
		Parameters:  json.RawMessage(`{"type":"object","properties":{"url_or_data":{"type":"string","description":"Image URL, data URI (data:image/...;base64,...), or image_id UUID"}},"required":["url_or_data"]}`),
		Fn: func(args map[string]any) string {
			urlOrData, _ := args["url_or_data"].(string)
			if urlOrData == "" {
				return "Error: url_or_data is required"
			}
			text, err := geminiClient.FetchAndAnalyzeImage(ctx, urlOrData)
			if err != nil {
				return "Image error: " + err.Error()
			}
			return text
		},
	}

	writeFile := ToolDef{
		Name:        "write_file",
		Description: "Write or update a text file in the session. The file is AUTOMATICALLY presented to the user.",
		Parameters:  json.RawMessage(`{"type":"object","properties":{"filename":{"type":"string"},"content":{"type":"string"}},"required":["filename","content"]}`),
		Fn: func(args map[string]any) string {
			filename, _ := args["filename"].(string)
			content, _ := args["content"].(string)
			if filename == "" {
				return "Error: filename required"
			}
			filename = filepath.Base(filename) // sanitize path
			mime := guessMime(filename)
			art := putArtifact(sessionID, filename, []byte(content), mime)
			emitFilePresent(eventCh, art)
			n := strings.Count(content, "\n") + 1
			return fmt.Sprintf("✅ Wrote '%s' (v%d, %d lines).", filename, art.Version, n)
		},
	}

	listWorkspace := ToolDef{
		Name:        "list_workspace",
		Description: "List all files currently in the session (written by the agent).",
		Parameters:  json.RawMessage(`{"type":"object","properties":{}}`),
		Fn: func(_ map[string]any) string {
			arts := listArtifacts(sessionID)
			if len(arts) == 0 {
				return "Session has no files."
			}
			var lines []string
			for _, a := range arts {
				lines = append(lines, fmt.Sprintf("  %s  (v%d, %dB, %s)", a.Filename, a.Version, len(a.Content), a.MimeType))
			}
			return "Session files:\n" + strings.Join(lines, "\n")
		},
	}

	return []ToolDef{loadSkill, webSearch, readImage, writeFile, listWorkspace}
}

// emitFilePresent sends file_present + artifact_content SSE events.
func emitFilePresent(eventCh chan<- SSEEvent, art *Artifact) {
	emit(eventCh, "file_present", map[string]any{
		"id":         art.ID,
		"filename":   art.Filename,
		"mime":       art.MimeType,
		"size":       len(art.Content),
		"line_count": art.LineCount(),
		"version":    art.Version,
	})
	if art.IsText {
		emit(eventCh, "artifact_content", map[string]any{
			"id":      art.ID,
			"content": art.TextContent(),
		})
	} else {
		emit(eventCh, "artifact_binary", map[string]any{
			"id":   art.ID,
			"data": base64.StdEncoding.EncodeToString(art.Content),
			"mime": art.MimeType,
		})
	}
}

func listSkills() []string {
	entries, err := os.ReadDir(skillsDir)
	if err != nil {
		return nil
	}
	var names []string
	for _, e := range entries {
		if !e.IsDir() && strings.HasSuffix(e.Name(), ".md") {
			names = append(names, strings.TrimSuffix(e.Name(), ".md"))
		}
	}
	return names
}

// ── JSONPlaceholder tools ──────────────────────────────────────────────────────

const jpBase = "https://jsonplaceholder.typicode.com"

var JSONPlaceholderTools = []ToolDef{
	{
		Name:        "list_users",
		Description: "List all 10 users from JSONPlaceholder API.",
		Parameters:  json.RawMessage(`{"type":"object","properties":{}}`),
		Fn:          toolListUsers,
	},
	{
		Name:        "get_user",
		Description: "Get full profile of a user by ID (1–10).",
		Parameters:  json.RawMessage(`{"type":"object","properties":{"user_id":{"type":"integer","description":"User ID 1-10"}},"required":["user_id"]}`),
		Fn:          toolGetUser,
	},
	{
		Name:        "get_posts",
		Description: "Get posts written by a user by user_id.",
		Parameters:  json.RawMessage(`{"type":"object","properties":{"user_id":{"type":"integer","description":"User ID"}},"required":["user_id"]}`),
		Fn:          toolGetPosts,
	},
	{
		Name:        "get_todos",
		Description: "Get todo list of a user (by user_id) with completion status.",
		Parameters:  json.RawMessage(`{"type":"object","properties":{"user_id":{"type":"integer","description":"User ID"}},"required":["user_id"]}`),
		Fn:          toolGetTodos,
	},
	{
		Name:        "get_comments",
		Description: "Get comments on a specific post (by post_id).",
		Parameters:  json.RawMessage(`{"type":"object","properties":{"post_id":{"type":"integer","description":"Post ID"}},"required":["post_id"]}`),
		Fn:          toolGetComments,
	},
}

func jpGet(path string) ([]byte, error) {
	resp, err := http.Get(jpBase + path) //nolint:noctx
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	return io.ReadAll(resp.Body)
}

func intArg(args map[string]any, key string) int {
	v, ok := args[key]
	if !ok {
		return 0
	}
	switch n := v.(type) {
	case float64:
		return int(n)
	case int:
		return n
	}
	return 0
}

func toolListUsers(_ map[string]any) string {
	data, err := jpGet("/users")
	if err != nil {
		return "Error: " + err.Error()
	}
	var users []map[string]any
	if json.Unmarshal(data, &users) != nil {
		return "Parse error"
	}
	var lines []string
	for _, u := range users {
		lines = append(lines, fmt.Sprintf("#%v: %v (@%v) — %v", u["id"], u["name"], u["username"], u["email"]))
	}
	return strings.Join(lines, "\n")
}

func toolGetUser(args map[string]any) string {
	id := intArg(args, "user_id")
	data, err := jpGet(fmt.Sprintf("/users/%d", id))
	if err != nil {
		return "Error: " + err.Error()
	}
	var u map[string]any
	if json.Unmarshal(data, &u) != nil || u["id"] == nil {
		return fmt.Sprintf("User %d not found.", id)
	}
	addr, _ := u["address"].(map[string]any)
	city := ""
	if addr != nil {
		city, _ = addr["city"].(string)
	}
	comp, _ := u["company"].(map[string]any)
	compName := ""
	if comp != nil {
		compName, _ = comp["name"].(string)
	}
	return fmt.Sprintf("#%v: %v (@%v)\nEmail: %v | Phone: %v | Website: %v\nCompany: %v | City: %v",
		u["id"], u["name"], u["username"], u["email"], u["phone"], u["website"], compName, city)
}

func toolGetPosts(args map[string]any) string {
	id := intArg(args, "user_id")
	data, err := jpGet(fmt.Sprintf("/posts?userId=%d", id))
	if err != nil {
		return "Error: " + err.Error()
	}
	var posts []map[string]any
	if json.Unmarshal(data, &posts) != nil {
		return "Parse error"
	}
	if len(posts) == 0 {
		return fmt.Sprintf("User %d has no posts.", id)
	}
	lines := []string{fmt.Sprintf("%d posts by user #%d:", len(posts), id)}
	for _, p := range posts {
		lines = append(lines, fmt.Sprintf("  [%v] %v", p["id"], p["title"]))
	}
	return strings.Join(lines, "\n")
}

func toolGetTodos(args map[string]any) string {
	id := intArg(args, "user_id")
	data, err := jpGet(fmt.Sprintf("/todos?userId=%d", id))
	if err != nil {
		return "Error: " + err.Error()
	}
	var todos []map[string]any
	if json.Unmarshal(data, &todos) != nil {
		return "Parse error"
	}
	if len(todos) == 0 {
		return fmt.Sprintf("User %d has no todos.", id)
	}
	done := 0
	for _, t := range todos {
		if c, _ := t["completed"].(bool); c {
			done++
		}
	}
	lines := []string{fmt.Sprintf("Todos of user #%d: %d/%d completed", id, done, len(todos))}
	for _, t := range todos {
		mark := "○"
		if c, _ := t["completed"].(bool); c {
			mark = "✓"
		}
		lines = append(lines, fmt.Sprintf("  %s [%v] %v", mark, t["id"], t["title"]))
	}
	return strings.Join(lines, "\n")
}

func toolGetComments(args map[string]any) string {
	id := intArg(args, "post_id")
	data, err := jpGet(fmt.Sprintf("/comments?postId=%d", id))
	if err != nil {
		return "Error: " + err.Error()
	}
	var comments []map[string]any
	if json.Unmarshal(data, &comments) != nil {
		return "Parse error"
	}
	if len(comments) == 0 {
		return fmt.Sprintf("Post %d has no comments.", id)
	}
	lines := []string{fmt.Sprintf("%d comments on post #%d:", len(comments), id)}
	for _, c := range comments {
		body, _ := c["body"].(string)
		if len(body) > 120 {
			body = body[:120] + "..."
		}
		lines = append(lines, fmt.Sprintf("  [%v] %v (%v): %v", c["id"], c["name"], c["email"], body))
	}
	return strings.Join(lines, "\n")
}

// ── Tool execution helpers ────────────────────────────────────────────────────

type toolResult struct {
	id     string
	name   string
	result string
}

// execToolsParallel executes all tool calls concurrently and returns results in original order.
func execToolsParallel(
	toolCalls []dsToolCall,
	toolMap map[string]ToolDef,
	eventCh chan<- SSEEvent,
	traceID, parentSpanID string,
) []toolResult {
	results := make([]toolResult, len(toolCalls))
	var wg sync.WaitGroup
	for i, tc := range toolCalls {
		wg.Add(1)
		go func(idx int, tc dsToolCall) {
			defer wg.Done()
			name := tc.Function.Name
			fmt.Printf("[agent] tool[%d]: %s(%s)\n", idx, name, truncate(tc.Function.Arguments, 120))
			emit(eventCh, "tool_call", map[string]string{"name": name})

			def, ok := toolMap[name]
			var result string
			if !ok {
				result = fmt.Sprintf("Unknown tool: %s", name)
			} else {
				var args map[string]any
				if json.Unmarshal([]byte(tc.Function.Arguments), &args) != nil {
					args = map[string]any{}
				}
				toolSpanID := lfUUID()
				globalLF.SpanStart(toolSpanID, traceID, parentSpanID, name, map[string]any{"args": truncate(tc.Function.Arguments, 300)})
				result = callWithRetry(def, args, eventCh)
				globalLF.SpanEnd(toolSpanID, traceID, map[string]any{"result": truncate(result, 300)})
			}
			emit(eventCh, "tool_result", map[string]any{
				"name":   name,
				"index":  idx,
				"result": truncateRune(result, 20000),
			})
			results[idx] = toolResult{id: tc.ID, name: name, result: result}
		}(i, tc)
	}
	wg.Wait()
	return results
}

func callWithRetry(def ToolDef, args map[string]any, ch chan<- SSEEvent) string {
	for attempt := 1; attempt <= 3; attempt++ {
		result := func() (r string) {
			defer func() {
				if rec := recover(); rec != nil {
					r = fmt.Sprintf("tool panic: %v", rec)
				}
			}()
			return def.Fn(args)
		}()
		if !strings.HasPrefix(result, "Error:") {
			return result
		}
		if attempt < 3 {
			emit(ch, "tool_retry", map[string]string{"info": fmt.Sprintf("%s (%d/3)", def.Name, attempt)})
		}
	}
	return fmt.Sprintf("[TOOL ERROR] %s failed after 3 attempts", def.Name)
}

// ── String helpers ────────────────────────────────────────────────────────────

func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n]
}

func truncateRune(s string, n int) string {
	if len(s) <= n {
		return s
	}
	for n > 0 && (s[n]&0xC0) == 0x80 {
		n--
	}
	return s[:n]
}

func contains(s []string, v string) bool {
	for _, x := range s {
		if x == v {
			return true
		}
	}
	return false
}

func fmtSize(bytes int) string {
	switch {
	case bytes >= 1<<20:
		return fmt.Sprintf("%.1f MB", float64(bytes)/float64(1<<20))
	case bytes >= 1<<10:
		return fmt.Sprintf("%d KB", bytes>>10)
	default:
		return fmt.Sprintf("%d B", bytes)
	}
}

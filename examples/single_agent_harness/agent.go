package main

import (
	"context"
	"fmt"
	"strings"
	"sync"
	"time"
)

const (
	maxReActRounds  = 12
	rootAgentSystem = `Bạn là trợ lý AI đa năng, xử lý mọi yêu cầu trong 1 luồng suy nghĩ liên tục.
Trả lời bằng tiếng Việt. Không bịa thông tin.

FILE & ARTIFACT MODEL:
Mọi file bạn viết/sửa/tạo ra ĐƯỢC TỰ ĐỘNG present cho user — KHÔNG paste code/content
vào câu trả lời text. Gọi đúng tool, file tự hiện trong UI panel.

## Skills có sẵn (gọi load_skill("<name>") để đọc chi tiết khi cần):

- jsonplaceholder — Truy vấn dữ liệu JSONPlaceholder API: users, posts, todos, comments.
  Dùng khi user hỏi về dữ liệu users/posts/todos/comments cụ thể từ JSONPlaceholder.

- vision_ocr — Quy trình đọc/phân tích ảnh chi tiết (OCR, trích xuất bảng, nhận diện vùng).
  Dùng khi cần đọc kỹ nội dung ảnh theo yêu cầu cụ thể. Câu hỏi đơn giản ("ảnh này là gì")
  có thể gọi read_image trực tiếp mà không cần load skill.

CHỈ load_skill khi câu hỏi THỰC SỰ thuộc domain đó. Đừng load "cho chắc". Nếu không
thuộc skill nào, trả lời trực tiếp bằng core tools hoặc kiến thức chung.

CORE TOOLS (luôn có sẵn, không cần load skill):
- load_skill(skill_name)                          → đọc tài liệu nghiệp vụ chi tiết cho 1 domain
- web_search(query)                               → tìm kiếm web, tin tức, giá cả, thông tin thực tế
- read_image(url_or_data)                         → phân tích ảnh bằng vision
  • Khi user message chứa [Ảnh đính kèm — gọi read_image("<id>")...], gọi read_image với id đó
- execute_python(code)                            → chạy Python trong sandbox Docker; tự lưu .last_run.py khi lỗi
- write_file(filename, content)                   → viết text file — TỰ ĐỘNG present
- write_code(filename, content)                   → alias của write_file — TỰ ĐỘNG present
- read_code(filename, start_line=1, end_line=500) → đọc file với line numbers
- execute_file(filename)                          → chạy file đã lưu trong Docker sandbox
- list_workspace()                                → liệt kê tất cả file trong session
- patch_code(filename, old_snippet, new_snippet)  → thay thế text trong file — TỰ ĐỘNG present
- grep_code(filename, pattern)                    → tìm kiếm regex trong file, trả về line numbers
- present_artifact(filename)                      → CHỈ dùng khi user nói "show lại"/"present lại"

SANDBOX ENVIRONMENT:
Có sẵn: pandas, openpyxl, matplotlib, numpy, python-docx (import docx),
  pdfminer.six (from pdfminer.high_level import extract_text), Pillow (PIL), requests
KHÔNG có network — pip install SẼ THẤT BẠI. Báo user thêm vào base image nếu cần.
File user upload có sẵn tại /uploaded/<filename> — đọc trực tiếp bằng pandas/open().

FILE GENERATION: Viết output vào /tmp/<filename>. File /tmp TỰ ĐỘNG present — KHÔNG gọi
present_artifact sau execute_python/execute_file.

FILE READING WORKFLOW:
1. grep_code(filename, 'heading') → lấy line number
2. read_code(filename, start_line=X, end_line=Y) → đọc đúng range

CODE ITERATION (khi lỗi — KHÔNG viết lại từ đầu):
1. Failing code tự lưu thành '.last_run.py'
2. read_code('.last_run.py') → xem lỗi
3. patch_code('.last_run.py', <old>, <fixed>) → sửa targeted
4. execute_file('.last_run.py') → re-run

ERROR RECOVERY:
- Tool lỗi → viết 1-2 câu giải thích TRƯỚC khi gọi tool tiếp.
- SyntaxError → grep_code tìm dòng lỗi, patch_code sửa.
- OSError read-only → patch_code chuyển write sang /tmp/.

Sau khi load_skill, các tool domain đó tự động active cho round tiếp theo trong turn này.
Có thể load nhiều skill trong 1 turn nếu câu hỏi đa domain.
`
)

// ── Session-level state ───────────────────────────────────────────────────────

var (
	sessionSkillsMu  sync.Mutex
	sessionSkillsMap = map[string]map[string]bool{} // sessionID → active skill names

	cancelsMu sync.Mutex
	cancels   = map[string]context.CancelFunc{}
)

func getSessionSkills(sessionID string) map[string]bool {
	sessionSkillsMu.Lock()
	defer sessionSkillsMu.Unlock()
	if _, ok := sessionSkillsMap[sessionID]; !ok {
		sessionSkillsMap[sessionID] = map[string]bool{}
	}
	return sessionSkillsMap[sessionID] // direct reference — mutations persist
}

func resetSessionSkills(sessionID string, keep map[string]bool) {
	sessionSkillsMu.Lock()
	defer sessionSkillsMu.Unlock()
	sessionSkillsMap[sessionID] = keep
}

func registerCancel(sessionID string, cancel context.CancelFunc) {
	cancelsMu.Lock()
	defer cancelsMu.Unlock()
	if old, ok := cancels[sessionID]; ok {
		old() // cancel any prior turn
	}
	cancels[sessionID] = cancel
}

func cancelSession(sessionID string) bool {
	cancelsMu.Lock()
	defer cancelsMu.Unlock()
	if fn, ok := cancels[sessionID]; ok {
		fn()
		delete(cancels, sessionID)
		return true
	}
	return false
}

func deregisterCancel(sessionID string) {
	cancelsMu.Lock()
	defer cancelsMu.Unlock()
	delete(cancels, sessionID)
}

// ── ReAct loop ────────────────────────────────────────────────────────────────

// runAgentTurn runs one agent turn (ReAct loop with tool calling).
// history is the full conversation so far (user + model turns, no tool results).
// eventCh receives SSE events; the caller drains it and writes to the HTTP response.
// Returns the agent's final text response.
func runAgentTurn(
	ctx context.Context,
	sessionID string,
	traceID string,
	history []messageJSON,
	eventCh chan<- SSEEvent,
) string {
	// Active skills persist across turns for this session.
	activeSkills := getSessionSkills(sessionID)

	// Build mutable tool map shared across rounds.
	// load_skill can inject new tools into this map mid-turn.
	toolMap := map[string]ToolDef{}
	tmRef := &toolMap

	// Core tools always available.
	core := makeCoreTools(ctx, sessionID, activeSkills, tmRef, eventCh)
	for _, t := range core {
		toolMap[t.Name] = t
	}
	// Pre-populate tools for skills already active in this session.
	for skill := range activeSkills {
		for _, t := range skillToolsMap[skill] {
			toolMap[t.Name] = t
		}
	}

	// Convert history to DeepSeek message format.
	msgs := make([]dsChatMsg, 0, len(history)+1)
	msgs = append(msgs, dsChatMsg{Role: "system", Content: strPtr(rootAgentSystem)})
	for _, m := range history {
		role := m.Role
		if role == "model" {
			role = "assistant"
		}
		msgs = append(msgs, dsChatMsg{Role: role, Content: strPtr(m.Content)})
	}

	// Langfuse: span covering the entire agent turn.
	spanID := lfUUID()
	lastUser := ""
	for i := len(history) - 1; i >= 0; i-- {
		if history[i].Role == "user" {
			lastUser = history[i].Content
			break
		}
	}
	globalLF.SpanStart(spanID, traceID, "", "root_agent", map[string]any{"query": truncate(lastUser, 300)})

	var fullText string
	var toolsCalled []string

	for round := 0; round < maxReActRounds; round++ {
		fmt.Printf("[agent] round %d session=%s skills=%v\n", round+1, sessionID[:8], activeSkillsList(activeSkills))

		// Rebuild tool list from current toolMap (may have grown via load_skill).
		activeDefs := activeToolDefs(toolMap)
		apiTools := buildAPITools(activeDefs)

		// Track which tool_call_start events we've already emitted per index.
		nameEmitted := map[int]bool{}

		genID := lfUUID()
		globalLF.GenerationStart(genID, traceID, spanID,
			fmt.Sprintf("round-%d", round+1), _agentModel,
			lfDSMsgs(msgs, 10))

		resp, promptTok, completionTok, firstDelta, err := dsClient.StreamChatWithTools(
			ctx, msgs, apiTools, nil,
			func(tok string) {
				emit(eventCh, "token", map[string]string{"text": tok})
			},
			func(idx int, name, argChunk string) {
				if name != "" && !nameEmitted[idx] {
					nameEmitted[idx] = true
					emit(eventCh, "tool_call_start", map[string]any{"name": name, "index": idx})
				}
				if argChunk != "" {
					emit(eventCh, "tool_arg_chunk", map[string]any{"index": idx, "chunk": argChunk})
				}
			},
		)
		if err != nil {
			globalLF.GenerationEnd(genID, traceID, map[string]any{"error": err.Error()}, promptTok, completionTok, time.Time{})
			globalLF.SpanEnd(spanID, traceID, map[string]any{"error": err.Error(), "tools_called": toolsCalled})
			if ctx.Err() != nil {
				return fullText // cancelled — return whatever we have
			}
			emit(eventCh, "error", map[string]string{"message": err.Error()})
			return fullText
		}

		// Output as plain {role, content} so Langfuse renders markdown instead of a table.
		// Tool call names are already visible as child spans — no need to duplicate here.
		assistantOut := map[string]any{"role": "assistant", "content": ""}
		if resp.Content != nil && *resp.Content != "" {
			assistantOut["content"] = truncate(*resp.Content, 3000)
		} else if len(resp.ToolCalls) > 0 {
			names := make([]string, len(resp.ToolCalls))
			for i, tc := range resp.ToolCalls {
				names[i] = tc.Function.Name
			}
			assistantOut["content"] = fmt.Sprintf("[tool calls: %v]", names)
		}
		globalLF.GenerationEnd(genID, traceID, assistantOut, promptTok, completionTok, firstDelta)

		emit(eventCh, "usage", map[string]any{
			"agent": "root", "prompt_tok": promptTok, "completion_tok": completionTok,
		})

		msgs = append(msgs, *resp)

		if len(resp.ToolCalls) == 0 {
			// Final answer.
			if resp.Content != nil {
				fullText = *resp.Content
			}
			break
		}

		// Execute tools (parallel).
		results := execToolsParallel(resp.ToolCalls, toolMap, eventCh, traceID, spanID)
		for _, r := range results {
			msgs = append(msgs, dsChatMsg{
				Role:       "tool",
				Content:    strPtr(r.result),
				ToolCallID: r.id,
			})
			if !contains(toolsCalled, r.name) {
				toolsCalled = append(toolsCalled, r.name)
			}
		}
	}

	globalLF.SpanEnd(spanID, traceID, map[string]any{
		"answer":       truncate(fullText, 300),
		"tools_called": toolsCalled,
	})

	if len(toolsCalled) > 0 {
		emit(eventCh, "tools_done", map[string]any{"tools": toolsCalled})
	}
	return fullText
}

// activeToolDefs returns all ToolDefs currently in the toolMap.
func activeToolDefs(toolMap map[string]ToolDef) []ToolDef {
	out := make([]ToolDef, 0, len(toolMap))
	for _, t := range toolMap {
		out = append(out, t)
	}
	return out
}

func activeSkillsList(m map[string]bool) []string {
	var out []string
	for k := range m {
		out = append(out, k)
	}
	return out
}

// ── History compaction ────────────────────────────────────────────────────────

// isStructuredMsg returns true for messages that contain structured tool state
// rather than narrative conversation (preserved verbatim during compact).
func isStructuredMsg(msg messageJSON) bool {
	return strings.Contains(msg.Content, "[TOOL_RESULT]") ||
		strings.Contains(msg.Content, "[STATE #") ||
		strings.Contains(msg.Content, "[STRUCTURED STATE")
}

// summarizeHistory compacts a history slice.
// structured messages are preserved (last 3 kept); narrative messages are LLM-summarized.
// onProgress is called with the number of chars generated so far (for SSE progress events).
// Returns (new history, tokens before, tokens after, error).
func summarizeHistory(
	ctx context.Context,
	history []messageJSON,
	sessionID string,
	onProgress func(generated int),
) ([]messageJSON, int, int, error) {
	var structured, narrative []messageJSON
	for _, m := range history {
		if isStructuredMsg(m) {
			structured = append(structured, m)
		} else {
			narrative = append(narrative, m)
		}
	}

	tokensBefore := estimateTokens(history)

	// Build prompt for narrative summarization.
	var flatParts []string
	for _, m := range narrative {
		role := strings.ToUpper(m.Role)
		flatParts = append(flatParts, fmt.Sprintf("[%s]: %s", role, m.Content))
	}
	flat := strings.Join(flatParts, "\n\n")

	prompt := "Tóm tắt ngắn gọn (150-250 từ) diễn biến cuộc hội thoại sau — KHÔNG cần " +
		"lặp lại số liệu chi tiết. Giữ lại:\n" +
		"  (1) User đã yêu cầu gì, theo trình tự\n" +
		"  (2) Agent đã quyết định/thực hiện gì để đáp ứng\n" +
		"  (3) Câu hỏi hoặc quyết định nào còn đang chờ user xác nhận\n" +
		"Không bịa, không thêm thông tin không có trong hội thoại.\n\n" +
		"CONVERSATION:\n" + flat

	var generated int
	summary, err := dsClient.Summarize(ctx, prompt, func(tok string) {
		generated += len(tok)
		if onProgress != nil {
			onProgress(generated)
		}
	})
	if err != nil {
		return nil, tokensBefore, 0, err
	}

	// Keep last 3 structured messages.
	var keptStructured []messageJSON
	if len(structured) > 3 {
		keptStructured = structured[len(structured)-3:]
	} else {
		keptStructured = structured
	}

	// Wrap structured messages.
	var wrappedParts []string
	for i, m := range keptStructured {
		wrappedParts = append(wrappedParts, fmt.Sprintf("[STATE #%d]: %s", i+1, m.Content))
	}

	newHistory := []messageJSON{}
	if len(wrappedParts) > 0 {
		newHistory = append(newHistory, messageJSON{
			Role:    "user",
			Content: "[STRUCTURED STATE — giữ nguyên 100%, KHÔNG suy diễn lại từ summary bên dưới]\n" + strings.Join(wrappedParts, "\n\n"),
		})
	}
	newHistory = append(newHistory,
		messageJSON{Role: "user", Content: "[LỊCH SỬ ĐÃ COMPACT — tóm tắt các turn trước]\n" + summary},
		messageJSON{Role: "model", Content: "Dạ em đã nắm context. Anh/chị cần em làm gì tiếp ạ?"},
	)

	// Evict skills not referenced in preserved state.
	preservedText := strings.Join(wrappedParts, " ") + summary
	sessionSkillsMu.Lock()
	active := sessionSkillsMap[sessionID]
	kept := map[string]bool{}
	for skill := range active {
		if strings.Contains(preservedText, "[SKILL LOADED: "+skill+"]") {
			kept[skill] = true
		}
	}
	sessionSkillsMap[sessionID] = kept
	evicted := []string{}
	for skill := range active {
		if !kept[skill] {
			evicted = append(evicted, skill)
		}
	}
	sessionSkillsMu.Unlock()
	if len(evicted) > 0 {
		fmt.Printf("[compact] session=%s evicted skills=%v\n", sessionID[:8], evicted)
	}

	tokensAfter := estimateTokens(newHistory)
	return newHistory, tokensBefore, tokensAfter, nil
}

// estimateTokens is a rough token estimate: ~4 chars per token (similar to Python's chars//4).
func estimateTokens(history []messageJSON) int {
	total := 0
	for _, m := range history {
		total += len(m.Content)
	}
	if total == 0 {
		return 0
	}
	return total / 4
}

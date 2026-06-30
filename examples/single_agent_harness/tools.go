package main

import (
	"archive/zip"
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"sync"

	"github.com/xuri/excelize/v2"
)

// ToolDef describes a callable function tool for DeepSeek function calling.
type ToolDef struct {
	Name        string
	Description string
	Parameters  json.RawMessage
	Fn          func(args map[string]any) string
	// NoRetry disables callWithRetry for this tool.
	// Set for deterministic tools where the same input always produces the same result
	// (e.g. code execution — retrying identical code is pointless and hides the real error).
	NoRetry bool
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

// makeCoreTools returns the tools always available to the agent.
// Ported faithfully from Python lab's _make_core_tools().
func makeCoreTools(
	ctx context.Context,
	sessionID string,
	activeSkills map[string]bool,
	toolMapRef *map[string]ToolDef,
	eventCh chan<- SSEEvent,
) []ToolDef {

	// ── load_skill ────────────────────────────────────────────────────────────
	loadSkill := ToolDef{
		Name:        "load_skill",
		Description: "Read detailed domain documentation before handling a domain-specific request. After loading, the domain's tools become available in subsequent rounds.",
		Parameters:  json.RawMessage(`{"type":"object","properties":{"skill_name":{"type":"string","description":"Name of the skill to load"}},"required":["skill_name"]}`),
		Fn: func(args map[string]any) string {
			name := strArg(args, "skill_name")
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
			if tm := *toolMapRef; tm != nil {
				for _, t := range skillToolsMap[name] {
					tm[t.Name] = t
				}
			}
			fmt.Printf("[skill] loaded '%s' (%d chars) for session %s\n", name, len(data), sessionID[:8])
			return fmt.Sprintf("[SKILL LOADED: %s]\n\n%s", name, string(data))
		},
	}

	// ── web_search ────────────────────────────────────────────────────────────
	webSearch := ToolDef{
		Name:        "web_search",
		Description: "Search the web for current information, news, prices, or real-world facts.",
		Parameters:  json.RawMessage(`{"type":"object","properties":{"query":{"type":"string","description":"Search query in Vietnamese or English"}},"required":["query"]}`),
		Fn: func(args map[string]any) string {
			query := strArg(args, "query")
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

	// ── read_image ────────────────────────────────────────────────────────────
	readImage := ToolDef{
		Name:        "read_image",
		Description: "Analyze an image using vision AI. Accepts a URL, base64 data URI, or a server-side image_id (UUID).",
		Parameters:  json.RawMessage(`{"type":"object","properties":{"url_or_data":{"type":"string","description":"Image URL, data URI (data:image/...;base64,...), or image_id UUID"}},"required":["url_or_data"]}`),
		Fn: func(args map[string]any) string {
			urlOrData := strArg(args, "url_or_data")
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

	// binaryExts: extensions blocked from write_file/write_code (binary files come from execute_python).
	binaryExts := map[string]bool{
		"png": true, "jpg": true, "jpeg": true, "gif": true, "webp": true,
		"pdf": true, "xlsx": true, "xls": true, "docx": true, "doc": true,
		"zip": true, "tar": true, "gz": true, "mp4": true, "mp3": true,
	}

	writeFileFn := func(args map[string]any) string {
		filename := filepath.Base(strArg(args, "filename"))
		content := strArg(args, "content")
		if filename == "" || filename == "." {
			return "Error: filename required"
		}
		ext := strings.ToLower(strings.TrimPrefix(filepath.Ext(filename), "."))
		if binaryExts[ext] {
			return fmt.Sprintf(
				"Error: '%s' is binary — do NOT use write_file for it. "+
					"Binary files are created by execute_python (save to /tmp, auto-exported). "+
					"Use present_artifact('%s') to re-present an existing file.",
				filename, filename)
		}
		mime := guessMime(filename)
		art := putArtifact(sessionID, filename, []byte(content), mime)
		emitFilePresent(eventCh, art)
		n := strings.Count(content, "\n") + 1
		return fmt.Sprintf("✅ Wrote '%s' (v%d, %d lines).", filename, art.Version, n)
	}

	// ── write_file ────────────────────────────────────────────────────────────
	writeFile := ToolDef{
		Name:        "write_file",
		Description: "Write or update a text file (code, markdown, CSV, JSON, …) in the session. Auto-presents to user.",
		Parameters:  json.RawMessage(`{"type":"object","properties":{"filename":{"type":"string"},"content":{"type":"string"}},"required":["filename","content"]}`),
		Fn:          writeFileFn,
	}

	// ── write_code — saves script silently (no file_present card) ───────────
	writeCodeFn := func(args map[string]any) string {
		filename := filepath.Base(strArg(args, "filename"))
		content := strArg(args, "content")
		if filename == "" || filename == "." {
			return "Error: filename required"
		}
		ext := strings.ToLower(strings.TrimPrefix(filepath.Ext(filename), "."))
		if binaryExts[ext] {
			return fmt.Sprintf(
				"Error: '%s' is binary — do NOT use write_code for it. "+
					"Binary files are created by execute_python (save to /tmp, auto-exported). "+
					"Use present_artifact('%s') to re-present an existing file.",
				filename, filename)
		}
		mime := guessMime(filename)
		art := putArtifact(sessionID, filename, []byte(content), mime)
		// Intentionally NOT calling emitFilePresent — scripts are internal artifacts;
		// the chip already shows the code preview during streaming.
		n := strings.Count(content, "\n") + 1
		return fmt.Sprintf("✅ Wrote '%s' (v%d, %d lines).", filename, art.Version, n)
	}
	writeCode := ToolDef{
		Name:        "write_code",
		Description: "Save or update a script (Python, shell, …) in the session. Does NOT present as download card — use execute_file to run it.",
		Parameters:  json.RawMessage(`{"type":"object","properties":{"filename":{"type":"string"},"content":{"type":"string"}},"required":["filename","content"]}`),
		Fn:          writeCodeFn,
	}

	// errorHint is appended when execute_python/execute_file fails with no exports.
	const errorHint = "\n\n💡 Failing code auto-saved as '.last_run.py'.\n" +
		"   → read('.last_run.py')                            — inspect with line numbers\n" +
		"   → grep('.last_run.py', pattern)                   — locate specific issue\n" +
		"   → patch('.last_run.py', old, new) + execute_file('.last_run.py') — targeted fix"

	// ── execute_python ────────────────────────────────────────────────────────
	executePython := ToolDef{
		NoRetry: true, // same code → same error; retry masks the real failure
		Name:    "execute_python",
		Description: "Execute Python code in an isolated Docker sandbox. Returns stdout+stderr (timeout 90s). " +
			"Preloaded: pandas, openpyxl, matplotlib, numpy, python-docx, pdfminer.six, Pillow, requests. " +
			"NO network — pip install will fail. " +
			"Files saved to /tmp are auto-exported as artifacts. " +
			"User uploads available at /uploaded/<filename>. " +
			"Failing code auto-saved as '.last_run.py'.",
		Parameters: json.RawMessage(`{"type":"object","properties":{"code":{"type":"string","description":"Complete Python source code (include all imports, print outputs)"}},"required":["code"]}`),
		Fn: func(args map[string]any) string {
			code := strArg(args, "code")
			if code == "" {
				return "Error: code is required"
			}
			fmt.Printf("[execute_python] %d chars session=%s\n", len(code), sessionID[:8])
			output, hasError := executeCode(ctx, "python", code, sessionID, eventCh)
			if hasError && sessionID != "" {
				if strings.Contains(output, "📎 Exported:") {
					output += "\n\n⚠️ Script exited with error but files were exported."
				} else {
					putArtifact(sessionID, ".last_run.py", []byte(code), "text/x-python")
					output += errorHint
				}
			}
			return output
		},
	}

	// ── execute_file ──────────────────────────────────────────────────────────
	executeFile := ToolDef{
		NoRetry:     true,
		Name:        "execute_file",
		Description: "Execute a saved session file in the Docker sandbox.",
		Parameters:  json.RawMessage(`{"type":"object","properties":{"filename":{"type":"string","description":"Filename in the session"}},"required":["filename"]}`),
		Fn: func(args map[string]any) string {
			filename := filepath.Base(strArg(args, "filename"))
			if filename == "" || filename == "." {
				return "Error: filename required"
			}
			art, errMsg := resolveTextArt(sessionID, filename)
			if errMsg != "" {
				return errMsg
			}
			lang := "python"
			if strings.HasSuffix(filename, ".sh") {
				lang = "bash"
			}
			fmt.Printf("[execute_file] %s (%d chars)\n", filename, len(art.Content))
			output, hasError := executeCode(ctx, lang, art.TextContent(), sessionID, eventCh)
			if hasError && sessionID != "" && !strings.Contains(output, "📎 Exported:") {
				putArtifact(sessionID, ".last_run.py", art.Content, "text/x-python")
				output += errorHint
			}
			return output
		},
	}

	// ── read ──────────────────────────────────────────────────────────────────
	readCode := ToolDef{
		Name:        "read",
		Description: "Read any session or uploaded text file with line numbers. Use start_line/end_line to read a specific range. For Excel files use read_excel instead.",
		Parameters:  json.RawMessage(`{"type":"object","properties":{"filename":{"type":"string","description":"Filename in session or uploaded files"},"start_line":{"type":"integer","description":"First line to read, 1-indexed (default 1)"},"end_line":{"type":"integer","description":"Last line to read inclusive (default 500)"}},"required":["filename"]}`),
		Fn: func(args map[string]any) string {
			filename := filepath.Base(strArg(args, "filename"))
			if filename == "" || filename == "." {
				return "Error: filename required"
			}
			startLine := intArgDefault(args, "start_line", 1)
			endLine := intArgDefault(args, "end_line", 500)
			if startLine < 1 {
				startLine = 1
			}

			// Session artifact first
			art := getArtifact(sessionID, filename)
			if art != nil {
				if !art.IsText {
					return fmt.Sprintf("'%s' là binary file (%s, %dB, v%d). Dùng present_artifact('%s') để xem.",
						filename, art.MimeType, len(art.Content), art.Version, filename)
				}
				return sliceWithLineNumbers(art.TextContent(), filename, fmt.Sprintf("v%d", art.Version), startLine, endLine)
			}

			// Uploaded file fallback
			upath := filepath.Join(sessionUploadDir(sessionID), filename)
			if data, err := os.ReadFile(upath); err == nil {
				ext := strings.ToLower(filepath.Ext(filename))
				uploadBinExts := map[string]bool{
					".xlsx": true, ".xls": true, ".pdf": true, ".zip": true,
					".tar": true, ".gz": true, ".docx": true, ".doc": true, ".pptx": true, ".ppt": true,
				}
				if uploadBinExts[ext] {
					fi, _ := os.Stat(upath)
					size := int64(0)
					if fi != nil {
						size = fi.Size()
					}
					return fmt.Sprintf("Uploaded binary file '%s' (%d bytes) — available read-only at /uploaded/%s in execute_python.",
						filename, size, filename)
				}
				return sliceWithLineNumbers(string(data), filename, "uploaded", startLine, endLine)
			}

			arts := listArtifacts(sessionID)
			artNames := make([]string, 0, len(arts))
			for _, a := range arts {
				artNames = append(artNames, a.Filename)
			}
			return fmt.Sprintf("File '%s' not found. Session: %v. Uploaded: %v", filename, artNames, listUploadedFiles(sessionID))
		},
	}

	// ── patch ─────────────────────────────────────────────────────────────────
	patchCode := ToolDef{
		Name:        "patch",
		Description: "Apply a targeted find-and-replace in any text file (code, markdown, CSV, …). old_snippet must match exactly — use grep first to get the exact text. Shows a red/green diff preview. Does NOT emit a download card.",
		Parameters:  json.RawMessage(`{"type":"object","properties":{"filename":{"type":"string","description":"Filename in session or uploaded files"},"old_snippet":{"type":"string","description":"Exact substring to replace — must exist verbatim in the file"},"new_snippet":{"type":"string","description":"Replacement text"}},"required":["filename","old_snippet","new_snippet"]}`),
		Fn: func(args map[string]any) string {
			filename := filepath.Base(strArg(args, "filename"))
			oldSnippet := strArg(args, "old_snippet")
			newSnippet := strArg(args, "new_snippet")
			if filename == "" || filename == "." || oldSnippet == "" {
				return "Error: filename and old_snippet required"
			}
			art, errMsg := resolveTextArt(sessionID, filename)
			if errMsg != "" {
				return errMsg
			}
			content := art.TextContent()
			if !strings.Contains(content, oldSnippet) {
				return "❌ Snippet not found in '" + filename + "'. Current content:\n" + addLineNumbers(content)
			}
			newContent := strings.Replace(content, oldSnippet, newSnippet, 1)
			if newContent == content {
				return fmt.Sprintf("⚠️ No change: old_snippet and new_snippet are identical in '%s'. Check indentation or whitespace.", filename)
			}
			newArt := putArtifact(sessionID, filename, []byte(newContent), art.MimeType)
			// Emit diff preview for the chip (no file_present card — scripts stay internal)
			emit(eventCh, "patch_diff", map[string]any{
				"filename":    filename,
				"old_snippet": oldSnippet,
				"new_snippet": newSnippet,
				"version":     newArt.Version,
			})
			n := strings.Count(newContent, "\n") + 1
			return fmt.Sprintf("✅ Patched '%s' → v%d (%d lines). Run execute_file('%s') to retry.", filename, newArt.Version, n, filename)
		},
	}

	// ── grep ──────────────────────────────────────────────────────────────────
	grepCode := ToolDef{
		Name:        "grep",
		Description: "Search for a literal string or regex in any text file (code, markdown, CSV, …). Returns matching lines with line numbers. Always grep before patch to confirm the exact old_snippet exists.",
		Parameters:  json.RawMessage(`{"type":"object","properties":{"filename":{"type":"string","description":"Filename in session or uploaded files"},"pattern":{"type":"string","description":"Literal string or regex pattern to search for"}},"required":["filename","pattern"]}`),
		Fn: func(args map[string]any) string {
			filename := filepath.Base(strArg(args, "filename"))
			pattern := strArg(args, "pattern")
			if filename == "" || filename == "." || pattern == "" {
				return "Error: filename and pattern required"
			}

			// Resolve content
			var text, sourceLabel string
			art := getArtifact(sessionID, filename)
			if art != nil {
				if !art.IsText {
					return fmt.Sprintf("'%s' is binary — cannot grep binary artifact.", filename)
				}
				text = art.TextContent()
				sourceLabel = fmt.Sprintf("v%d", art.Version)
			} else {
				upath := filepath.Join(sessionUploadDir(sessionID), filename)
				data, err := os.ReadFile(upath)
				if err != nil {
					arts := listArtifacts(sessionID)
					artNames := make([]string, 0, len(arts))
					for _, a := range arts {
						artNames = append(artNames, a.Filename)
					}
					return fmt.Sprintf("File '%s' not found. Session: %v. Uploaded: %v", filename, artNames, listUploadedFiles(sessionID))
				}
				ext := strings.ToLower(filepath.Ext(filename))
				grepBinExts := map[string]bool{
					".xlsx": true, ".xls": true, ".pdf": true, ".zip": true,
					".tar": true, ".gz": true, ".docx": true, ".doc": true,
				}
				if grepBinExts[ext] {
					return fmt.Sprintf("'%s' is binary — cannot grep binary artifact.", filename)
				}
				text = string(data)
				sourceLabel = "uploaded"
			}

			// Search
			lines := strings.Split(text, "\n")
			var matches []string
			rx, rxErr := regexp.Compile(pattern)
			for i, line := range lines {
				var hit bool
				if rxErr == nil {
					hit = rx.MatchString(line)
				} else {
					hit = strings.Contains(line, pattern)
				}
				if hit {
					matches = append(matches, fmt.Sprintf("%4d: %s", i+1, line))
				}
			}
			if len(matches) == 0 {
				return fmt.Sprintf("No matches for '%s' in '%s' (%s).", pattern, filename, sourceLabel)
			}
			return fmt.Sprintf("Matches in '%s' (%s, %d lines):\n%s", filename, sourceLabel, len(matches), strings.Join(matches, "\n"))
		},
	}

	// ── list_workspace ────────────────────────────────────────────────────────
	listWorkspace := ToolDef{
		Name:        "list_workspace",
		Description: "List all files in the session (written by agent, exported from Docker, or uploaded by user).",
		Parameters:  json.RawMessage(`{"type":"object","properties":{}}`),
		Fn: func(_ map[string]any) string {
			arts := listArtifacts(sessionID)
			uploaded := listUploadedFiles(sessionID)
			if len(arts) == 0 && len(uploaded) == 0 {
				return "Session has no files."
			}
			var lines []string
			for _, a := range arts {
				lines = append(lines, fmt.Sprintf("  %-30s (v%d, %s, %s)", a.Filename, a.Version, fmtSize(len(a.Content)), a.MimeType))
			}
			for _, name := range uploaded {
				upath := filepath.Join(sessionUploadDir(sessionID), name)
				size := 0
				if fi, err := os.Stat(upath); err == nil {
					size = int(fi.Size())
				}
				lines = append(lines, fmt.Sprintf("  /uploaded/%-20s (%s) [uploaded]", name, fmtSize(size)))
			}
			return "Session files:\n" + strings.Join(lines, "\n")
		},
	}

	// ── present_artifact ──────────────────────────────────────────────────────
	presentArtifact := ToolDef{
		Name:        "present_artifact",
		Description: "Re-present an existing session file to the user. Use ONLY when user explicitly says 'show again' / 'present again'.",
		Parameters:  json.RawMessage(`{"type":"object","properties":{"filename":{"type":"string","description":"Filename to re-present"}},"required":["filename"]}`),
		Fn: func(args map[string]any) string {
			filename := filepath.Base(strArg(args, "filename"))
			if filename == "" || filename == "." {
				return "Error: filename required"
			}
			art := getArtifact(sessionID, filename)
			if art == nil {
				arts := listArtifacts(sessionID)
				var names []string
				for _, a := range arts {
					names = append(names, a.Filename)
				}
				if len(names) == 0 {
					return fmt.Sprintf("File '%s' not found. Session has no files.", filename)
				}
				return fmt.Sprintf("File '%s' not found. Available: %s", filename, strings.Join(names, ", "))
			}
			emitFilePresent(eventCh, art)
			return fmt.Sprintf("✅ Re-presented '%s' (v%d, %dB).", art.Filename, art.Version, len(art.Content))
		},
	}

	// ── write_binary_file ────────────────────────────────────────────────────
	writeBinaryFile := ToolDef{
		Name:        "write_binary_file",
		Description: "Write a binary file (xlsx, pdf, zip, png, …) already encoded as base64. Use when you have raw bytes to store directly — auto-presents.",
		Parameters:  json.RawMessage(`{"type":"object","properties":{"filename":{"type":"string"},"base64_content":{"type":"string"}},"required":["filename","base64_content"]}`),
		Fn: func(args map[string]any) string {
			filename := strArg(args, "filename")
			b64 := strArg(args, "base64_content")
			if filename == "" || b64 == "" {
				return "Error: filename and base64_content required."
			}
			raw, err := base64.StdEncoding.DecodeString(b64)
			if err != nil {
				raw, err = base64.RawStdEncoding.DecodeString(b64)
				if err != nil {
					return "Error: invalid base64 — " + err.Error()
				}
			}
			mime := guessMime(filepath.Base(filename))
			art := putArtifact(sessionID, filepath.Base(filename), raw, mime)
			emitFilePresent(eventCh, art)
			return fmt.Sprintf("✅ Wrote and presented '%s' (v%d, %dB).", art.Filename, art.Version, len(art.Content))
		},
	}

	// ── zip_files ─────────────────────────────────────────────────────────────
	zipFiles := ToolDef{
		Name:        "zip_files",
		Description: "Pack multiple session files into a single .zip and present it. Use when the user asks to bundle/download all files.",
		Parameters:  json.RawMessage(`{"type":"object","properties":{"filenames":{"type":"array","items":{"type":"string"}},"zip_name":{"type":"string"}},"required":["filenames","zip_name"]}`),
		Fn: func(args map[string]any) string {
			zipName := strArg(args, "zip_name")
			if zipName == "" {
				zipName = "archive.zip"
			}
			var filenames []string
			if raw, ok := args["filenames"]; ok {
				switch v := raw.(type) {
				case []any:
					for _, item := range v {
						if s, ok := item.(string); ok {
							filenames = append(filenames, s)
						}
					}
				case []string:
					filenames = v
				}
			}
			if len(filenames) == 0 {
				return "Error: filenames list is empty."
			}
			var buf bytes.Buffer
			zw := zip.NewWriter(&buf)
			var packed, missing []string
			for _, fname := range filenames {
				art := getArtifact(sessionID, filepath.Base(fname))
				if art == nil {
					missing = append(missing, fname)
					continue
				}
				w, err := zw.Create(art.Filename)
				if err != nil {
					continue
				}
				w.Write(art.Content) //nolint:errcheck
				packed = append(packed, art.Filename)
			}
			zw.Close()
			if len(packed) == 0 {
				return fmt.Sprintf("Không tìm thấy file nào: %v", missing)
			}
			zipArt := putArtifact(sessionID, zipName, buf.Bytes(), "application/zip")
			emitFilePresent(eventCh, zipArt)
			msg := fmt.Sprintf("✅ Đã đóng gói %d file vào '%s' và present.", len(packed), zipName)
			if len(missing) > 0 {
				msg += fmt.Sprintf(" Không tìm thấy: %v.", missing)
			}
			return msg
		},
	}

	// ── edit_xlsx ─────────────────────────────────────────────────────────────
	editXlsx := ToolDef{
		Name: "edit_xlsx",
		Description: "Confirm an Excel file is accessible in the Docker sandbox at /uploaded/<filename>, " +
			"then describe the edit to perform. Works for both user-uploaded files AND session artifacts. " +
			"Call execute_python() with the openpyxl code immediately after.",
		Parameters: json.RawMessage(`{"type":"object","properties":{"filename":{"type":"string"},"instruction":{"type":"string"}},"required":["filename","instruction"]}`),
		Fn: func(args map[string]any) string {
			filename := strArg(args, "filename")
			instruction := strArg(args, "instruction")
			if filename == "" {
				return "Error: filename required."
			}
			fname := filepath.Base(filename)

			// Case 1: session artifact (agent-created file).
			if art := getArtifact(sessionID, fname); art != nil {
				udir := sessionUploadDir(sessionID)
				if err := os.MkdirAll(udir, 0o755); err != nil {
					return "Error creating upload dir: " + err.Error()
				}
				dest := filepath.Join(udir, fname)
				if err := os.WriteFile(dest, art.Content, 0o644); err != nil {
					return "Error staging file: " + err.Error()
				}
				return fmt.Sprintf(
					"File '%s' (v%d, %dB) đã stage vào /uploaded/%s trong Docker.\n"+
						"Hướng dẫn: %s\n"+
						"Bước tiếp: execute_python() đọc từ '/uploaded/%s', lưu vào '/tmp/%s'.",
					fname, art.Version, len(art.Content), fname, instruction, fname, fname,
				)
			}

			// Case 2: user-uploaded file (already on disk, already mounted as /uploaded/).
			upath := filepath.Join(sessionUploadDir(sessionID), fname)
			if info, err := os.Stat(upath); err == nil {
				return fmt.Sprintf(
					"File '%s' (%dB) là file user upload — đã có sẵn tại /uploaded/%s trong Docker (không cần stage).\n"+
						"Hướng dẫn: %s\n"+
						"Bước tiếp: execute_python() đọc từ '/uploaded/%s', lưu output vào '/tmp/%s'.",
					fname, info.Size(), fname, instruction, fname, fname,
				)
			}

			// Not found anywhere.
			var sessionNames []string
			for _, a := range listArtifacts(sessionID) {
				sessionNames = append(sessionNames, a.Filename)
			}
			var uploadedNames []string
			if entries, _ := os.ReadDir(sessionUploadDir(sessionID)); entries != nil {
				for _, e := range entries {
					uploadedNames = append(uploadedNames, e.Name())
				}
			}
			return fmt.Sprintf(
				"File '%s' không tìm thấy.\nSession artifacts: %v\nUploaded files: %v",
				fname, sessionNames, uploadedNames,
			)
		},
	}

	// ── read_excel ────────────────────────────────────────────────────────────
	readExcel := ToolDef{
		Name: "read_excel",
		Description: "Đọc nhanh nội dung file Excel (.xlsx/.xls) — KHÔNG cần Docker, KHÔNG cần viết code. " +
			"Dùng cho: xem có mấy sheet, preview dữ liệu, xem cấu trúc cột, merged cells, đọc vài dòng đầu. " +
			"Nếu cần TÍNH TOÁN, lọc phức tạp, hoặc SỬA file → dùng execute_python hoặc edit_xlsx.",
		Parameters: json.RawMessage(`{"type":"object","properties":{"filename":{"type":"string"},"sheet_name":{"type":"string"},"max_rows":{"type":"integer"}},"required":["filename"]}`),
		Fn: func(args map[string]any) string {
			filename := strArg(args, "filename")
			sheetName := strArg(args, "sheet_name")
			maxRows := intArgDefault(args, "max_rows", 50)
			if maxRows <= 0 {
				maxRows = 50
			}
			if filename == "" {
				return "Error: filename required."
			}
			fname := filepath.Base(filename)

			// Resolve bytes: session artifact first, then uploaded file.
			var raw []byte
			if art := getArtifact(sessionID, fname); art != nil {
				raw = art.Content
			} else {
				upath := filepath.Join(sessionUploadDir(sessionID), fname)
				var err error
				raw, err = os.ReadFile(upath)
				if err != nil {
					var artNames []string
					for _, a := range listArtifacts(sessionID) {
						artNames = append(artNames, a.Filename)
					}
					return fmt.Sprintf("File '%s' không tìm thấy. Session: %v. Uploaded: check /uploaded/.", fname, artNames)
				}
			}

			f, err := excelize.OpenReader(bytes.NewReader(raw))
			if err != nil {
				return fmt.Sprintf("Lỗi đọc file Excel: %v. File có thể bị hỏng hoặc không đúng format xlsx.", err)
			}
			defer f.Close()

			sheets := f.GetSheetList()
			var out strings.Builder
			fmt.Fprintf(&out, "Workbook '%s': %d sheet(s): %v\n", fname, len(sheets), sheets)

			sheetsToRead := sheets
			if sheetName != "" {
				sheetsToRead = []string{sheetName}
			}

			for _, sname := range sheetsToRead {
				rows, err := f.GetRows(sname)
				if err != nil {
					fmt.Fprintf(&out, "\n## Sheet '%s': lỗi đọc — %v\n", sname, err)
					continue
				}
				fmt.Fprintf(&out, "\n## Sheet: %s\n", sname)

				// Report merged cells so agent knows about them before writing openpyxl code.
				merges, _ := f.GetMergeCells(sname)
				if len(merges) > 0 {
					fmt.Fprintf(&out, "[Merged cells: ")
					for i, m := range merges {
						if i > 0 {
							out.WriteString(", ")
						}
						fmt.Fprintf(&out, "%s→%s", m.GetStartAxis(), m.GetEndAxis())
					}
					out.WriteString("]\n")
				}

				rowCount := 0
				for _, row := range rows {
					if rowCount >= maxRows {
						fmt.Fprintf(&out, "... [đã đọc %d dòng, còn nữa — tăng max_rows nếu cần]\n", maxRows)
						break
					}
					if len(row) == 0 {
						continue
					}
					out.WriteString(strings.Join(row, "\t"))
					out.WriteByte('\n')
					rowCount++
				}
				fmt.Fprintf(&out, "[%d dòng đã đọc trong sheet này]\n", rowCount)
			}
			return out.String()
		},
	}

	// ── end_conversation ─────────────────────────────────────────────────────────
	endConversation := ToolDef{
		Name: "end_conversation",
		Description: "Permanently ends the current conversation — the user will NOT be able to send any more messages in this session. " +
			"ONLY use in 2 cases: " +
			"(1) User has been persistently abusive AFTER a clear prior warning was issued in a previous turn, " +
			"(2) User explicitly requests ending AND has confirmed they understand this is permanent. " +
			"NEVER use if user shows any sign of self-harm, crisis, or intent to harm others — " +
			"regardless of how abusive they are being.",
		Parameters: json.RawMessage(`{"type":"object","properties":{}}`),
		Fn: func(args map[string]any) string {
			emit(eventCh, "conversation_ended", map[string]any{"session_id": sessionID})
			fmt.Printf("[agent] end_conversation called for session=%s\n", sessionID[:8])
			return "Conversation permanently ended."
		},
	}

	return []ToolDef{
		loadSkill, webSearch, readImage,
		writeFile, writeCode, writeBinaryFile, zipFiles, editXlsx,
		readExcel, executePython, executeFile,
		readCode, patchCode, grepCode,
		listWorkspace, presentArtifact,
		endConversation,
	}
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

func strArg(args map[string]any, key string) string {
	s, _ := args[key].(string)
	return s
}

func intArgDefault(args map[string]any, key string, def int) int {
	v, ok := args[key]
	if !ok {
		return def
	}
	switch n := v.(type) {
	case float64:
		return int(n)
	case int:
		return n
	}
	return def
}

// resolveTextArt resolves a filename to a text artifact.
// Session registry is checked first; uploaded text files are auto-promoted so
// patch_code / execute_file can operate on them.
func resolveTextArt(sessionID, filename string) (*Artifact, string) {
	if art := getArtifact(sessionID, filename); art != nil {
		if !art.IsText {
			return nil, fmt.Sprintf("'%s' là binary file — dùng execute_python để đọc.", filename)
		}
		return art, ""
	}

	upath := filepath.Join(sessionUploadDir(sessionID), filename)
	data, err := os.ReadFile(upath)
	if err != nil {
		arts := listArtifacts(sessionID)
		artNames := make([]string, 0, len(arts))
		for _, a := range arts {
			artNames = append(artNames, a.Filename)
		}
		return nil, fmt.Sprintf("File '%s' not found. Session: %v. Uploaded: %v",
			filename, artNames, listUploadedFiles(sessionID))
	}

	ext := strings.ToLower(filepath.Ext(filename))
	uploadBinExts := map[string]bool{
		".xlsx": true, ".xls": true, ".pdf": true, ".zip": true,
		".tar": true, ".gz": true, ".docx": true, ".doc": true, ".pptx": true, ".ppt": true,
	}
	if uploadBinExts[ext] {
		return nil, fmt.Sprintf("'%s' is a binary upload — use execute_python to process it at /uploaded/%s.", filename, filename)
	}

	// Auto-promote text upload to session registry.
	art := putArtifact(sessionID, filename, data, guessMime(filename))
	return art, ""
}

func listUploadedFiles(sessionID string) []string {
	entries, err := os.ReadDir(sessionUploadDir(sessionID))
	if err != nil {
		return nil
	}
	names := make([]string, 0, len(entries))
	for _, e := range entries {
		if !e.IsDir() {
			names = append(names, e.Name())
		}
	}
	return names
}

func sliceWithLineNumbers(content, filename, label string, startLine, endLine int) string {
	lines := strings.Split(content, "\n")
	total := len(lines)
	if endLine <= 0 || endLine > total {
		endLine = total
	}
	if startLine < 1 {
		startLine = 1
	}
	if startLine > total {
		return fmt.Sprintf("'%s' (%s) has %d lines — start_line %d out of range.", filename, label, total, startLine)
	}
	selected := lines[startLine-1 : endLine]
	var sb strings.Builder
	fmt.Fprintf(&sb, "'%s' (%s) lines %d-%d / %d total:\n", filename, label, startLine, endLine, total)
	for i, line := range selected {
		fmt.Fprintf(&sb, "%4d: %s\n", startLine+i, line)
	}
	return sb.String()
}

func addLineNumbers(content string) string {
	lines := strings.Split(content, "\n")
	var sb strings.Builder
	for i, line := range lines {
		fmt.Fprintf(&sb, "%4d: %s\n", i+1, line)
	}
	return sb.String()
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
				globalLF.SpanStart(toolSpanID, traceID, parentSpanID, name, map[string]any{"args": truncate(tc.Function.Arguments, 2000)})
				result = callWithRetry(def, args, eventCh)
				globalLF.SpanEnd(toolSpanID, traceID, map[string]any{"result": truncate(result, 5000)})
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
	call := func() (r string) {
		defer func() {
			if rec := recover(); rec != nil {
				r = fmt.Sprintf("tool panic: %v", rec)
			}
		}()
		return def.Fn(args)
	}

	// Deterministic tools (code execution, file ops) — never retry; return real error to LLM.
	if def.NoRetry {
		return call()
	}

	// Transient tools (web_search, HTTP calls) — retry up to 3x on "Error:" prefix.
	var last string
	for attempt := 1; attempt <= 3; attempt++ {
		last = call()
		if !strings.HasPrefix(last, "Error:") {
			return last
		}
		if attempt < 3 {
			emit(ch, "tool_retry", map[string]string{"info": fmt.Sprintf("%s (%d/3)", def.Name, attempt)})
		}
	}
	return last // return the actual error, not a generic wrapper
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

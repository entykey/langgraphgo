package main

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"strings"
	"time"
)

// parseBrief extracts a TaskBrief from tool args.
// DeepSeek sometimes passes task_brief as a JSON-encoded string instead of an object —
// this helper handles both forms so the tool never gets an empty brief.
func parseBrief(args map[string]any) TaskBrief {
	var brief TaskBrief
	switch v := args["task_brief"].(type) {
	case map[string]any:
		b, _ := json.Marshal(v)
		json.Unmarshal(b, &brief) //nolint:errcheck
	case string:
		// LLM double-encoded the object as a JSON string — decode it.
		json.Unmarshal([]byte(v), &brief) //nolint:errcheck
	}
	return brief
}

// makeReadImageTool returns a ToolDef that calls Gemini Vision with a TaskBrief.
// Supervisor calls this instead of routing to vision_agent node.
// Pattern mirrors makeWebSearchTool: tool calls Gemini, returns string for supervisor to synthesise.
func makeReadImageTool(ctx context.Context, gemini *GeminiClient, eventCh chan<- SSEEvent) ToolDef {
	return ToolDef{
		Name: "read_image",
		Description: `Giao cho Gemini Vision đọc/phân tích một file ảnh.
QUAN TRỌNG: Phải điền đầy đủ task_brief — Gemini Vision KHÔNG có context của conversation,
nó chỉ biết những gì bạn mô tả trong brief.
- Nếu user hỏi về vùng cụ thể: điền focus_areas chính xác
- Nếu user cần extract data: mô tả output_format rõ ràng
- Luôn điền success_criteria để Vision biết mức độ chi tiết cần thiết
Brief mờ nhạt = kết quả chung chung = user frustrated.`,
		Parameters: json.RawMessage(`{
			"type": "object",
			"properties": {
				"file_id": {
					"type": "string",
					"description": "ID của file ảnh (từ FileRegistry trong context)"
				},
				"task_brief": {
					"type": "object",
					"description": "Mô tả đầy đủ công việc cần Vision thực hiện",
					"properties": {
						"task": {"type": "string", "description": "Mô tả chính xác công việc"},
						"user_intent": {"type": "string", "description": "User đang cố làm gì với kết quả này"},
						"focus_areas": {"type": "array", "items": {"type": "string"}, "description": "Vùng cần chú ý đặc biệt"},
						"output_format": {"type": "string", "description": "Format output. Ví dụ: 'Markdown table', 'danh sách key-value'"},
						"success_criteria": {"type": "array", "items": {"type": "string"}, "description": "Tiêu chí kết quả tốt"},
						"rules": {"type": "array", "items": {"type": "string"}, "description": "Rules bắt buộc"}
					},
					"required": ["task", "user_intent", "output_format", "success_criteria"]
				}
			},
			"required": ["file_id", "task_brief"]
		}`),
		Fn: func(args map[string]any) string {
			fileID, _ := args["file_id"].(string)
			if fileID == "" {
				return "Error: file_id required"
			}

			brief := parseBrief(args)
			if brief.Task == "" {
				return "Error: task_brief.task is required — provide a detailed task description"
			}

			fmt.Printf("[read_image] file_id=%s task=%s\n", fileID, truncate(brief.Task, 80))

			b64, mime, ok := getImageForLLM(fileID)
			if !ok {
				return fmt.Sprintf("Error: image %s not found (expired or server restarted)", fileID)
			}

			systemPrompt := buildVisionSystemPrompt(brief)
			userMsg := fmt.Sprintf("Thực hiện task sau:\n\n%s\n\nUser đang cần kết quả để: %s",
				brief.Task, brief.UserIntent)
			msgs := []Message{{Role: "user", Content: userMsg}}

			text, promptTok, completionTok, _, err := gemini.StreamChatWithImage(
				ctx, systemPrompt, msgs, b64, mime,
				func(tok string) {
					emit(eventCh, "tool_stream", map[string]string{"name": "read_image", "text": tok})
				},
			)
			if err != nil {
				return "Error calling Vision: " + err.Error()
			}
			emit(eventCh, "usage", map[string]any{
				"agent": "vision_agent", "prompt_tok": promptTok, "completion_tok": completionTok,
			})
			return text
		},
	}
}

// buildVisionSystemPrompt converts a TaskBrief into a focused system prompt for Gemini Vision.
func buildVisionSystemPrompt(brief TaskBrief) string {
	var sb strings.Builder
	sb.WriteString("Bạn là Vision Agent chuyên trích xuất thông tin từ ảnh.\n")
	sb.WriteString("Thực hiện ĐÚNG task được giao, không làm gì thêm ngoài task.\n\n")

	if len(brief.FocusAreas) > 0 {
		sb.WriteString("## Vùng cần chú ý:\n")
		for _, area := range brief.FocusAreas {
			fmt.Fprintf(&sb, "- %s\n", area)
		}
		sb.WriteString("\n")
	}

	sb.WriteString("## Output format yêu cầu:\n")
	sb.WriteString(brief.OutputFormat)
	sb.WriteString("\n\n")

	sb.WriteString("## Tiêu chí thành công:\n")
	for _, c := range brief.SuccessCriteria {
		fmt.Fprintf(&sb, "- %s\n", c)
	}

	if len(brief.Rules) > 0 {
		sb.WriteString("\n## Rules bắt buộc:\n")
		for _, r := range brief.Rules {
			fmt.Fprintf(&sb, "- %s\n", r)
		}
	}

	if len(brief.SpecificFields) > 0 {
		sb.WriteString("\n## Chỉ cần extract các fields sau:\n")
		for _, f := range brief.SpecificFields {
			fmt.Fprintf(&sb, "- %s\n", f)
		}
	}

	sb.WriteString("\nNHẮC LẠI: Không summarize chung chung. Thực hiện đúng task và output format đã mô tả.")
	return sb.String()
}

// makeReadFileTool returns a ToolDef that dispatches document reading by MIME type.
// xlsx/docx/csv → Python subprocess; PDF → pdftotext (fallback: pdfminer via Python).
func makeReadFileTool(ctx context.Context, eventCh chan<- SSEEvent) ToolDef {
	return ToolDef{
		Name: "read_file",
		Description: `Đọc nội dung file document (xlsx, pdf, docx, csv).
Dùng khi user hỏi về dữ liệu trong file, cần extract bảng/text, hoặc cần compare với file khác.
Điền specific_fields nếu chỉ cần một số cột/sections để giảm output size.`,
		Parameters: json.RawMessage(`{
			"type": "object",
			"properties": {
				"file_id": {
					"type": "string",
					"description": "ID của file (từ FileRegistry trong context)"
				},
				"task_brief": {
					"type": "object",
					"properties": {
						"task": {"type": "string"},
						"user_intent": {"type": "string"},
						"output_format": {"type": "string"},
						"success_criteria": {"type": "array", "items": {"type": "string"}},
						"specific_fields": {
							"type": "array",
							"items": {"type": "string"},
							"description": "Columns/sheets/sections cần extract. Empty = extract tất cả"
						}
					},
					"required": ["task", "user_intent", "output_format", "success_criteria"]
				}
			},
			"required": ["file_id", "task_brief"]
		}`),
		Fn: func(args map[string]any) string {
			fileID, _ := args["file_id"].(string)
			if fileID == "" {
				return "Error: file_id required"
			}

			brief := parseBrief(args)
			fmt.Printf("[read_file] file_id=%s task=%s\n", fileID, truncate(brief.Task, 80))

			// supervisor already emitted tool_call before invoking this Fn.
			// Emit tool_stream progress token so the UI chip shows activity.
			emit(eventCh, "tool_stream", map[string]string{"name": "read_file", "text": ""})

			data, storedMime, err := fetchDocument(fileID)
			if err != nil {
				return "Error fetching file: " + err.Error()
			}

			tmpFile, err := os.CreateTemp("", "doc_*")
			if err != nil {
				return "Error creating temp file: " + err.Error()
			}
			defer os.Remove(tmpFile.Name())
			tmpFile.Write(data) //nolint:errcheck
			tmpFile.Close()

			// Use MIME from upload — far more reliable than magic-byte detection for
			// ZIP-based Office formats (xlsx and docx both start with PK bytes).
			mime := storedMime
			if mime == "" || mime == "application/octet-stream" {
				mime = detectMimeByContent(data)
			}

			fmt.Printf("[read_file] dispatch mime=%s path=%s\n", mime, tmpFile.Name())

			var result string
			switch {
			case isSpreadsheet(mime):
				result = execReadXLSX(tmpFile.Name(), brief)
			case isPDF(mime):
				result = execReadPDF(ctx, tmpFile.Name(), brief)
			case isWord(mime):
				result = execReadDOCX(tmpFile.Name(), brief)
			case mime == "text/csv":
				result = execReadCSV(tmpFile.Name(), brief)
			default:
				// Plain text fallback
				result = string(data)
				if len(result) > 8000 {
					result = result[:8000] + "\n...[truncated]"
				}
			}

			return result
		},
	}
}

// detectMimeByContent uses magic bytes — intentionally simple.
// Caller should prefer storedMime from upload when available.
func detectMimeByContent(data []byte) string {
	if len(data) < 4 {
		return "application/octet-stream"
	}
	// ZIP-based (xlsx, docx) — prefer storedMime; return zip as generic fallback.
	if data[0] == 'P' && data[1] == 'K' {
		return "application/zip"
	}
	if bytes.HasPrefix(data, []byte("%PDF")) {
		return "application/pdf"
	}
	return "application/octet-stream"
}

// ── Python exec helpers ───────────────────────────────────────────────────────

func execReadXLSX(path string, brief TaskBrief) string {
	script := fmt.Sprintf(`import sys

path = %q
errors = []

# Method 1: openpyxl (modern xlsx/xlsm, including digitally signed files)
try:
    import openpyxl
    wb = openpyxl.load_workbook(path, read_only=True, data_only=True)
    names = wb.sheetnames
    print(f"Sheets ({len(names)}): {names}")
    for sn in names:
        ws = wb[sn]
        print(f"\n## Sheet: {sn}")
        count = 0
        for row in ws.iter_rows(values_only=True):
            if any(c is not None for c in row):
                print("\t".join(str(c) if c is not None else "" for c in row))
                count += 1
        print(f"[{count} rows]")
    sys.exit(0)
except Exception as e:
    errors.append(f"openpyxl: {e}")

# Method 2: xlrd 1.2.0 (legacy .xls and non-standard .xlsx e.g. Vietnamese invoices)
try:
    import xlrd
    wb = xlrd.open_workbook(path)
    names = wb.sheet_names()
    print(f"Sheets ({len(names)}): {names}")
    for sn in names:
        ws = wb.sheet_by_name(sn)
        print(f"\n## Sheet: {sn}")
        for i in range(ws.nrows):
            print("\t".join(str(v) for v in ws.row_values(i)))
        print(f"[{ws.nrows} rows]")
    sys.exit(0)
except Exception as e:
    errors.append(f"xlrd: {e}")

print("; ".join(errors), file=sys.stderr)
sys.exit(1)
`, path)
	return runPythonScript(script, 30)
}

func execReadPDF(ctx context.Context, path string, brief TaskBrief) string {
	// Try pdftotext first (fast, no Python needed).
	tctx, cancel := context.WithTimeout(ctx, 15*time.Second)
	defer cancel()
	out, err := exec.CommandContext(tctx, "pdftotext", "-layout", path, "-").Output()
	if err == nil && len(strings.TrimSpace(string(out))) > 0 {
		result := string(out)
		if len(result) > 10000 {
			result = result[:10000] + "\n...[truncated at 10000 chars]"
		}
		return result
	}

	// Fallback: pdfminer via Python.
	script := fmt.Sprintf(`# Task: %s
from pdfminer.high_level import extract_text
text = extract_text(%q)
print(text[:10000] if len(text) > 10000 else text)
`, brief.Task, path)
	return runPythonScript(script, 30)
}

func execReadDOCX(path string, brief TaskBrief) string {
	script := fmt.Sprintf(`# Task: %s
from docx import Document
doc = Document(%q)
for para in doc.paragraphs:
    if para.text.strip():
        print(para.text)
for i, table in enumerate(doc.tables):
    print(f"\n## Table {i+1}")
    for row in table.rows:
        cells = [cell.text.strip() for cell in row.cells]
        print("\t".join(cells))
`, brief.Task, path)
	return runPythonScript(script, 30)
}

func execReadCSV(path string, brief TaskBrief) string {
	script := fmt.Sprintf(`# Task: %s
import csv, sys
with open(%q, newline='', encoding='utf-8-sig') as f:
    reader = csv.reader(f)
    rows = list(reader)
print(f"CSV: {len(rows)} rows x {len(rows[0]) if rows else 0} cols")
for row in rows[:100]:
    print('\t'.join(row))
if len(rows) > 100:
    print(f"...[{len(rows)-100} more rows truncated]")
`, brief.Task, path)
	return runPythonScript(script, 15)
}

// runPythonScript writes the script to a temp file, executes it with agentPythonBin (venv),
// and returns stdout. Stderr is returned with a "ScriptError:" prefix so the supervisor
// can reason about the failure and potentially retry differently.
func runPythonScript(script string, timeoutSec int) string {
	f, err := os.CreateTemp("", "agent_*.py")
	if err != nil {
		return "Error: cannot create temp file: " + err.Error()
	}
	defer os.Remove(f.Name())
	f.WriteString(script) //nolint:errcheck
	f.Close()

	ctx, cancel := context.WithTimeout(context.Background(), time.Duration(timeoutSec)*time.Second)
	defer cancel()

	cmd := exec.CommandContext(ctx, agentPythonBin, f.Name())
	// Force UTF-8 I/O so Vietnamese characters don't crash on Windows (cp1252 default).
	cmd.Env = append(os.Environ(), "PYTHONIOENCODING=utf-8", "PYTHONUTF8=1")
	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr

	if err := cmd.Run(); err != nil {
		errMsg := stderr.String()
		if errMsg == "" {
			errMsg = err.Error()
		}
		if len(errMsg) > 500 {
			errMsg = errMsg[:500] + "...[truncated]"
		}
		return fmt.Sprintf("ScriptError:\n%s", errMsg)
	}

	result := stdout.String()
	if len(result) > 12000 {
		result = result[:12000] + "\n...[output truncated at 12000 chars]"
	}
	return result
}

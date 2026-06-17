// mas_agent_handoff — Go BE + Vite SPA multi-agent lab
//
// Graph: supervisor → json_agent (DeepSeek) | web_search (Gemini) | self-reply → END
// Streams SSE events: node_start, routing, tool_call, tool_retry, tools_done,
//
//	citations, token, token_batch, done, error
//
// Run:
//
//	export GOOGLE_API_KEY=xxx DEEPSEEK_API_KEY=yyy
//	cd examples && go run ./mas_agent_handoff/
//
// Frontend (dev):
//
//	cd examples/mas_agent_handoff/fe && npm install && npm run dev
package main

import (
	"bufio"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"

	"github.com/smallnest/langgraphgo/graph"
)

// supervisorModel is set at startup by buildGraph based on SUPERVISOR_BACKEND env.
var supervisorModel = "gemini-3.1-flash-lite"

// serverSessionID is generated once at startup so every `go run .` gets its own
// named Langfuse session, distinct from the Python version using the same infra creds.
var serverSessionID string

const (
	jsonAgentModel = "deepseek-v4-flash"
	searchModel    = "gemini-3.1-flash-lite"
	listenAddr     = ":8080"
)

// ── .env loading ──────────────────────────────────────────────────────────────

func loadDotEnvFile(path string) bool {
	f, err := os.Open(path)
	if err != nil {
		return false
	}
	defer f.Close()
	sc := bufio.NewScanner(f)
	for sc.Scan() {
		line := strings.TrimSpace(sc.Text())
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		kv := strings.SplitN(line, "=", 2)
		if len(kv) != 2 {
			continue
		}
		k := strings.TrimSpace(kv[0])
		raw := kv[1]
		// strip inline comment (unquoted # and everything after)
		if i := strings.Index(raw, " #"); i >= 0 {
			raw = raw[:i]
		}
		v := strings.Trim(strings.TrimSpace(raw), `"'`)
		if os.Getenv(k) == "" {
			os.Setenv(k, v) //nolint:errcheck
		}
	}
	return true
}

func loadDotEnv() {
	if cwd, err := os.Getwd(); err == nil {
		if loadDotEnvFile(filepath.Join(cwd, ".env")) {
			return
		}
	}
	if exe, err := os.Executable(); err == nil {
		if loadDotEnvFile(filepath.Join(filepath.Dir(exe), ".env")) {
			return
		}
	}
	if cwd, err := os.Getwd(); err == nil {
		dir := cwd
		for i := 0; i < 4; i++ {
			parent := filepath.Dir(dir)
			if parent == dir {
				break
			}
			dir = parent
			if loadDotEnvFile(filepath.Join(dir, ".env")) {
				return
			}
		}
	}
}

func mustEnv(key string) string {
	v := os.Getenv(key)
	if v == "" {
		fmt.Fprintf(os.Stderr, "❌  %s not set. Create a .env file or export the variable.\n", key)
		os.Exit(1)
	}
	return v
}

// ── HTTP types ────────────────────────────────────────────────────────────────

type chatRequest struct {
	Message      string        `json:"message"`
	SessionID    string        `json:"session_id"`
	History      []messageJSON `json:"history"`
	FileRegistry []FileEntry   `json:"file_registry,omitempty"` // accumulated across turns
	// Backward-compat image fields (single image sent inline or resolved from upload).
	ImageID   string `json:"image_id,omitempty"`
	ImageB64  string `json:"image_b64,omitempty"`
	ImageMime string `json:"image_mime,omitempty"`
}

type messageJSON struct {
	Role    string `json:"role"`
	Content string `json:"content"`
	Name    string `json:"name,omitempty"`
}

// ── Main ──────────────────────────────────────────────────────────────────────

func main() {
	loadDotEnv()
	serverSessionID = "go_mas_agent_handoff_" + lfUUID()
	initLangfuse()
	initMinio()
	initPythonEnv()

	gemini := NewGeminiClient(mustEnv("GOOGLE_API_KEY"))
	ds := NewDSClient(mustEnv("DEEPSEEK_API_KEY"))

	g, err := buildGraph(gemini, ds)
	if err != nil {
		fmt.Fprintln(os.Stderr, "build graph:", err)
		os.Exit(1)
	}

	mux := http.NewServeMux()
	mux.HandleFunc("/config", corsMiddleware(configHandler))
	mux.HandleFunc("/upload", corsMiddleware(uploadHandler))
	mux.HandleFunc("/upload-doc", corsMiddleware(uploadDocHandler))
	mux.HandleFunc("/image/", corsMiddleware(imageHandler))
	mux.HandleFunc("/chat", corsMiddleware(chatHandler(g)))
	mux.Handle("/", staticHandler())

	fmt.Printf("[mas_agent_handoff] listening on http://localhost%s\n", listenAddr)
	if err := http.ListenAndServe(listenAddr, mux); err != nil {
		fmt.Fprintln(os.Stderr, "server:", err)
		globalLF.Shutdown()
		os.Exit(1)
	}
}

// ── Handlers ──────────────────────────────────────────────────────────────────

func configHandler(w http.ResponseWriter, _ *http.Request) {
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Cache-Control", "no-store")
	json.NewEncoder(w).Encode(map[string]string{ //nolint:errcheck
		"supervisor": supervisorModel,
		"json_agent": jsonAgentModel,
		"search":     searchModel,
		"session_id": serverSessionID,
	})
}

func chatHandler(g *graph.StateRunnable[AgentState]) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
			return
		}

		// Limit request body to 20 MB — large enough for a high-res image (base64 ≈ 4/3 × raw).
		// Prevents OOM from accidentally huge uploads; Gemini itself rejects > ~20 MB requests.
		r.Body = http.MaxBytesReader(w, r.Body, 20<<20)

		var req chatRequest
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			http.Error(w, "request too large or malformed", http.StatusRequestEntityTooLarge)
			return
		}

		// Resolve image bytes for the LLM (backward compat for inline/upload image).
		imageB64, imageMime := req.ImageB64, req.ImageMime
		if req.ImageID != "" {
			if b64, mime, ok := getImageForLLM(req.ImageID); ok {
				imageB64 = b64
				imageMime = mime
				fmt.Printf("[chat] resolved image_id=%s (%s)\n", req.ImageID, mime)
			} else {
				fmt.Printf("[chat] image_id=%s not found (expired or server restarted)\n", req.ImageID)
			}
		}

		// Backward-compat: promote a bare ImageID into the FileRegistry so the
		// new tool-based flow can read it without a separate upload-doc call.
		fileRegistry := req.FileRegistry
		if req.ImageID != "" {
			alreadyInRegistry := false
			for _, fe := range fileRegistry {
				if fe.ID == req.ImageID {
					alreadyInRegistry = true
					break
				}
			}
			if !alreadyInRegistry {
				sizeKB := len(imageB64) * 3 / 4 / 1024
				fileRegistry = append(fileRegistry, FileEntry{
					ID:     req.ImageID,
					Name:   "Ảnh đính kèm",
					Mime:   imageMime,
					SizeKB: sizeKB,
					Status: "available",
				})
			}
		}

		// Build message history.
		msgs := make([]Message, 0, len(req.History)+1)
		for _, m := range req.History {
			msgs = append(msgs, Message{Role: m.Role, Content: m.Content, Name: m.Name})
		}
		userContent := req.Message
		if req.ImageID != "" {
			userContent += fmt.Sprintf("\n[Ảnh đính kèm — gọi read_image(file_id: \"%s\") để phân tích ảnh này]", req.ImageID)
		}
		// Append hints for ALL image files in registry not yet mentioned in
		// userContent or in any prior history message — handles multi-image uploads.
		historyText := ""
		for _, m := range req.History {
			historyText += m.Content
		}
		for _, fe := range fileRegistry {
			if !strings.HasPrefix(fe.Mime, "image/") {
				continue
			}
			if strings.Contains(userContent, fe.ID) || strings.Contains(historyText, fe.ID) {
				continue
			}
			userContent += fmt.Sprintf("\n[Ảnh đính kèm: \"%s\" — gọi read_image(file_id: \"%s\") để phân tích]", fe.Name, fe.ID)
		}
		msgs = append(msgs, Message{Role: "user", Content: userContent})

		// Langfuse trace for this turn
		traceID := lfUUID()
		sessionID := req.SessionID
		if sessionID == "" {
			sessionID = serverSessionID
		}
		tags := []string{"mas-agent-handoff", "go", supervisorModel}
		globalLF.TraceCreate(traceID, sessionID, req.Message, tags)

		// SSE response headers
		w.Header().Set("Content-Type", "text/event-stream")
		w.Header().Set("Cache-Control", "no-cache")
		w.Header().Set("X-Accel-Buffering", "no")
		w.WriteHeader(http.StatusOK)
		flusher, canFlush := w.(http.Flusher)

		eventCh := make(chan SSEEvent, 512)

		go func() {
			defer close(eventCh)
			state := AgentState{
				Messages:      msgs,
				FileRegistry:  fileRegistry,
				ArtifactStore: make(map[string]string),
				EventCh:       eventCh,
				TraceID:       traceID,
				SessionID:     sessionID,
				ImageB64:      imageB64,
				ImageMime:     imageMime,
			}
			result, err := g.Invoke(r.Context(), state)
			if err != nil {
				eventCh <- SSEEvent{Type: "error", Data: map[string]string{"message": err.Error()}}
				return
			}
			// Update trace output — non-blocking, fires after done SSE is already sent
			if len(result.Messages) > 0 {
				last := result.Messages[len(result.Messages)-1]
				globalLF.TraceUpdate(traceID, last.Content)
			}
		}()

		for ev := range eventCh {
			fmt.Fprint(w, ev.Encode())
			if canFlush {
				flusher.Flush()
			}
		}

		fmt.Fprint(w, SSEEvent{Type: "done", Data: map[string]any{}}.Encode())
		if canFlush {
			flusher.Flush()
		}
	}
}

// uploadDocHandler accepts multipart/form-data with a "file" field for non-image documents
// (xlsx, pdf, docx, csv, txt). Returns a FileEntry JSON the frontend keeps in its registry.
func uploadDocHandler(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	r.Body = http.MaxBytesReader(w, r.Body, 20<<20)
	if err := r.ParseMultipartForm(20 << 20); err != nil {
		http.Error(w, "too large or malformed", http.StatusRequestEntityTooLarge)
		return
	}
	file, hdr, err := r.FormFile("file")
	if err != nil {
		http.Error(w, "missing 'file' field", http.StatusBadRequest)
		return
	}
	defer file.Close()

	data, err := io.ReadAll(file)
	if err != nil {
		http.Error(w, "read error", http.StatusInternalServerError)
		return
	}

	mime := hdr.Header.Get("Content-Type")
	if mime == "" || mime == "application/octet-stream" {
		mime = detectMimeByExt(hdr.Filename)
	}

	id := lfUUID()
	if err := storeDocument(id, mime, data); err != nil {
		http.Error(w, "storage error: "+err.Error(), http.StatusInternalServerError)
		return
	}

	entry := FileEntry{
		ID:     id,
		Name:   hdr.Filename,
		Mime:   mime,
		SizeKB: len(data) / 1024,
		Status: "available",
	}

	fmt.Printf("[upload-doc] %s (%s, %dKB) → %s\n", hdr.Filename, mime, entry.SizeKB, id)

	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(entry) //nolint:errcheck
}

func staticHandler() http.Handler {
	// Try to find the fe/dist directory relative to common run locations
	candidates := []string{
		"fe/dist",
		"examples/mas_agent_handoff/fe/dist",
		"mas_agent_handoff/fe/dist",
	}
	for _, p := range candidates {
		if info, err := os.Stat(p); err == nil && info.IsDir() {
			fmt.Printf("[mas_agent_handoff] serving static files from %s\n", p)
			fs := http.FileServer(http.Dir(p))
			return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				// Prevent browser caching of HTML so code changes are always picked up.
				if r.URL.Path == "/" || r.URL.Path == "/index.html" {
					w.Header().Set("Cache-Control", "no-store, no-cache, must-revalidate")
					w.Header().Set("Pragma", "no-cache")
				}
				fs.ServeHTTP(w, r)
			})
		}
	}
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprintln(w, "MAS Agent Handoff API is running.")
		fmt.Fprintln(w, "Build the frontend: cd examples/mas_agent_handoff/fe && npm install && npm run build")
	})
}

func corsMiddleware(next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Access-Control-Allow-Origin", "*")
		w.Header().Set("Access-Control-Allow-Headers", "Content-Type")
		w.Header().Set("Access-Control-Allow-Methods", "GET, POST, OPTIONS")
		if r.Method == http.MethodOptions {
			w.WriteHeader(http.StatusNoContent)
			return
		}
		next(w, r)
	}
}

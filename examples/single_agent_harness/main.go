// single_agent_harness — single-agent ReAct loop (DeepSeek) + Gemini VLM/Search
//
// Run:
//
//	cd examples && go run ./single_agent_harness/
//
// Frontend: served from static/index.html (copied from Python lab).
package main

import (
	"bufio"
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"strings"
	"sync"
	"syscall"
	"time"
)

const listenAddr = ":8080"

var (
	geminiClient *GeminiClient
	dsClient     *DSClient
	_agentModel  string
	_searchModel string
)

// ── Entry point ───────────────────────────────────────────────────────────────

func main() {
	loadDotEnv()
	initSkillsDir()
	initAgentLog()
	initKafka()
	initMongo()

	_agentModel = getEnv("AGENT_MODEL", "deepseek-v4-flash")
	_searchModel = getEnv("SEARCH_MODEL", "gemini-3.1-flash-lite")

	initLangfuse()
	initMinio()
	initCodeExec()

	geminiClient = NewGeminiClient(mustEnv("GOOGLE_API_KEY"), _searchModel)
	dsClient = NewDSClient(mustEnv("DEEPSEEK_API_KEY"), _agentModel)

	fmt.Printf("[single_agent_harness] listening on http://localhost%s\n", listenAddr)
	fmt.Printf("  agent  = deepseek/%s\n", _agentModel)
	fmt.Printf("  search = gemini/%s\n", _searchModel)
	fmt.Printf("  skills = %s\n", skillsDir)

	// Flush Kafka on Ctrl+C / SIGTERM before exit.
	go func() {
		sig := make(chan os.Signal, 1)
		signal.Notify(sig, os.Interrupt, syscall.SIGTERM)
		<-sig
		fmt.Fprintln(os.Stderr, "\n[sah] shutting down…")
		shutdownKafka()
		shutdownMongo()
		globalLF.Shutdown()
		os.Exit(0)
	}()

	mux := http.NewServeMux()
	mux.HandleFunc("/chat", corsMiddleware(chatHandler))
	mux.HandleFunc("/compact", corsMiddleware(compactHandler))
	mux.HandleFunc("/stop", corsMiddleware(stopHandler))
	mux.HandleFunc("/upload-image", corsMiddleware(uploadImageJSONHandler))
	mux.HandleFunc("/upload-file", corsMiddleware(uploadFileHandler))
	mux.HandleFunc("/upload", corsMiddleware(uploadHandler)) // multipart (from image_cache.go)
	mux.HandleFunc("/image/", corsMiddleware(imageHandler))
	mux.HandleFunc("/artifact/", corsMiddleware(artifactHandler))
	mux.HandleFunc("/config", corsMiddleware(configHandler))
	mux.HandleFunc("/sessions", corsMiddleware(sessionsListHandler))
	mux.HandleFunc("/sessions/", corsMiddleware(sessionDetailHandler))
	mux.Handle("/", staticHandler())

	if err := http.ListenAndServe(listenAddr, mux); err != nil {
		fmt.Fprintln(os.Stderr, "server:", err)
		shutdownKafka()
		shutdownMongo()
		globalLF.Shutdown()
		os.Exit(1)
	}
}

// ── Chat endpoint ──────────────────────────────────────────────────────────────

type chatRequest struct {
	Message   string        `json:"message"`
	SessionID string        `json:"session_id"`
	RequestID string        `json:"request_id"`
	History   []messageJSON `json:"history"`
}

func chatHandler(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	r.Body = http.MaxBytesReader(w, r.Body, 20<<20)

	var req chatRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		http.Error(w, "bad request: "+err.Error(), http.StatusBadRequest)
		return
	}
	if req.SessionID == "" {
		req.SessionID = lfUUID()
	}
	if req.RequestID == "" {
		req.RequestID = lfUUID()
	}
	sessionID := req.SessionID

	// Idempotency: skip duplicate requests.
	if isDuplicate(sessionID, req.RequestID) {
		w.Header().Set("Content-Type", "text/event-stream")
		w.Header().Set("Cache-Control", "no-cache")
		w.Header().Set("X-Accel-Buffering", "no")
		w.WriteHeader(http.StatusOK)
		fmt.Fprint(w, SSEEvent{Type: "session_status", Data: map[string]any{
			"session_id": sessionID,
			"status":     "generating",
			"duplicate":  true,
		}}.Encode())
		if f, ok := w.(http.Flusher); ok {
			f.Flush()
		}
		return
	}

	// Load authoritative ds_messages from MongoDB.
	// Fall back to building fresh if session doesn't exist yet.
	sl := loadSessionForTurn(sessionID)
	var dsMsgs []dsChatMsg
	var sessionName string
	if sl.Exists && len(sl.DSMessages) > 0 {
		dsMsgs = sl.DSMessages
		sessionName = sl.Name
		// Always use current system prompt (in case it changed).
		if len(dsMsgs) > 0 && dsMsgs[0].Role == "system" {
			dsMsgs[0] = dsChatMsg{Role: "system", Content: strPtr(rootAgentSystem)}
		}
	} else {
		dsMsgs = []dsChatMsg{{Role: "system", Content: strPtr(rootAgentSystem)}}
		runes := []rune(req.Message)
		if len(runes) > 60 {
			runes = runes[:60]
		}
		sessionName = string(runes)
	}

	// Append new user message.
	dsMsgs = append(dsMsgs, dsChatMsg{Role: "user", Content: strPtr(req.Message)})

	// Persist user turn immediately (before generation starts).
	if err := upsertUserTurn(sessionID, sessionName, req.RequestID, req.Message, dsMsgs); err != nil {
		fmt.Fprintf(os.Stderr, "[mongo] upsert user turn: %v\n", err)
	}

	// SSE headers.
	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("X-Accel-Buffering", "no")
	w.WriteHeader(http.StatusOK)
	flusher, canFlush := w.(http.Flusher)

	// Emit session_status so frontend updates sidebar immediately.
	fmt.Fprint(w, SSEEvent{Type: "session_status", Data: map[string]any{
		"session_id": sessionID,
		"name":       sessionName,
		"status":     "generating",
	}}.Encode())
	if canFlush {
		flusher.Flush()
	}

	eventCh := make(chan SSEEvent, 512)

	// CRITICAL: context.Background() so client disconnect does NOT kill generation.
	ctx, cancel := context.WithCancel(context.Background())
	registerCancel(sessionID, cancel)

	traceID := lfUUID()
	globalLF.TraceCreate(traceID, sessionID, req.Message, []string{"single-agent-harness", "go", _agentModel})

	genDone := make(chan struct{})
	var hbWG sync.WaitGroup
	hbWG.Add(1)

	// Heartbeat goroutine — prevents client disconnect timeout during slow generation.
	go func() {
		defer hbWG.Done()
		ticker := time.NewTicker(10 * time.Second)
		defer ticker.Stop()
		for {
			select {
			case <-ticker.C:
				select {
				case eventCh <- SSEEvent{Type: "heartbeat", Data: map[string]any{}}:
				case <-genDone:
					return
				}
			case <-genDone:
				return
			}
		}
	}()

	// Agent goroutine — runs to completion regardless of client connection.
	go func() {
		defer func() {
			deregisterCancel(sessionID)
			cancel()
			close(genDone)
			hbWG.Wait()
			close(eventCh)
		}()

		answer, hitMaxRounds, finalDSMsgs := runAgentTurn(ctx, sessionID, traceID, dsMsgs, eventCh)

		stopReason := "completed"
		switch {
		case ctx.Err() != nil:
			stopReason = consumeStopReason(sessionID)
		case answer == "" && hitMaxRounds:
			stopReason = "max_rounds"
		case answer == "":
			stopReason = "llm_error"
		}

		fmt.Printf("[agent] session=%s stop=%s answer_len=%d\n", sessionID[:8], stopReason, len(answer))
		if err := upsertAssistantTurn(sessionID, answer, finalDSMsgs, stopReason); err != nil {
			fmt.Fprintf(os.Stderr, "[mongo] upsert assistant turn: %v\n", err)
		}
		globalLF.TraceUpdate(traceID, answer)

		emit(eventCh, "done", map[string]any{"text": answer, "stop_reason": stopReason})
	}()

	// Drain SSE events and forward to client.
	const idleTimeout = 150 * time.Second
	var overflow *SSEEvent

	for {
		var ev SSEEvent
		if overflow != nil {
			ev = *overflow
			overflow = nil
		} else {
			var ok bool
			if ev, ok = recvTimeout(eventCh, idleTimeout); !ok {
				break
			}
			if ev.Type == "" {
				fmt.Fprint(w, SSEEvent{Type: "error", Data: map[string]string{"message": "idle timeout"}}.Encode())
				if canFlush {
					flusher.Flush()
				}
				break
			}
		}

		if ev.Type == "token" {
			text, _ := ev.Data.(map[string]string)["text"]
			for {
				next, ok := tryRecv(eventCh)
				if !ok {
					break
				}
				if next.Type == "token" {
					text += next.Data.(map[string]string)["text"]
				} else {
					overflow = &next
					break
				}
			}
			fmt.Fprint(w, SSEEvent{Type: "token", Data: map[string]string{"text": text}}.Encode())
			if canFlush {
				flusher.Flush()
			}
			continue
		}

		fmt.Fprint(w, ev.Encode())
		if canFlush {
			flusher.Flush()
		}
	}
}

// recvTimeout reads one event from ch with a timeout.
// Returns (ev, true) on success, (zero, false) if ch is closed,
// or ({Type: ""}, true) on timeout.
func recvTimeout(ch <-chan SSEEvent, timeout time.Duration) (SSEEvent, bool) {
	select {
	case ev, ok := <-ch:
		if !ok {
			return SSEEvent{}, false
		}
		return ev, true
	case <-time.After(timeout):
		return SSEEvent{}, true // timeout sentinel: Type == ""
	}
}

func tryRecv(ch <-chan SSEEvent) (SSEEvent, bool) {
	select {
	case ev := <-ch:
		return ev, true
	default:
		return SSEEvent{}, false
	}
}

// ── Compact endpoint ──────────────────────────────────────────────────────────

type compactRequest struct {
	SessionID string        `json:"session_id"`
	History   []messageJSON `json:"history"`
}

func compactHandler(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	var req compactRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		http.Error(w, "bad request", http.StatusBadRequest)
		return
	}
	if len(req.History) == 0 {
		writeSSE(w, "error", map[string]string{"message": "history is empty"})
		return
	}

	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	w.WriteHeader(http.StatusOK)
	flusher, canFlush := w.(http.Flusher)

	sseFlush := func(evType string, data any) {
		fmt.Fprint(w, SSEEvent{Type: evType, Data: data}.Encode())
		if canFlush {
			flusher.Flush()
		}
	}

	tokensBefore := estimateTokens(req.History)
	sseFlush("progress", map[string]any{"phase": "start", "tokens_before": tokensBefore, "generated": 0})

	t0 := time.Now()
	sessionID := req.SessionID
	if sessionID == "" {
		sessionID = "unknown"
	}

	newHistory, tokensBefore, tokensAfter, err := summarizeHistory(
		r.Context(),
		req.History,
		sessionID,
		func(generated int) {
			sseFlush("progress", map[string]any{"phase": "summarizing", "generated": generated})
		},
	)
	if err != nil {
		sseFlush("error", map[string]string{"message": err.Error()})
		return
	}

	compactMS := float64(time.Since(t0).Milliseconds())
	reductionPct := 0.0
	if tokensBefore > 0 {
		reductionPct = (1 - float64(tokensAfter)/float64(tokensBefore)) * 100
	}

	fmt.Printf("[compact] session=%s before=%d after=%d saved=%d (%.0f%%) in %.0fms\n",
		sessionID[:8], tokensBefore, tokensAfter, tokensBefore-tokensAfter, reductionPct, compactMS)

	sseFlush("done", map[string]any{
		"history":       newHistory,
		"tokens_before": tokensBefore,
		"tokens_after":  tokensAfter,
		"saved_tokens":  tokensBefore - tokensAfter,
		"reduction_pct": reductionPct,
		"compact_ms":    compactMS,
	})
}

// ── Stop endpoint ─────────────────────────────────────────────────────────────

func stopHandler(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	sessionID := r.URL.Query().Get("session_id")
	found := cancelSession(sessionID)
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(map[string]any{"ok": true, "found": found}) //nolint:errcheck
}

// ── Upload-image (JSON data_uri) ──────────────────────────────────────────────

type uploadImageJSONReq struct {
	DataURI string `json:"data_uri"`
}

func uploadImageJSONHandler(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	r.Body = http.MaxBytesReader(w, r.Body, 20<<20)

	var req uploadImageJSONReq
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		http.Error(w, "bad request", http.StatusBadRequest)
		return
	}
	if !strings.HasPrefix(req.DataURI, "data:") {
		http.Error(w, "data_uri must start with 'data:'", http.StatusBadRequest)
		return
	}

	parts := strings.SplitN(req.DataURI, ",", 2)
	if len(parts) != 2 {
		http.Error(w, "malformed data_uri", http.StatusBadRequest)
		return
	}
	header := strings.Split(parts[0], ";")[0]
	mime := strings.TrimPrefix(header, "data:")
	b64data := parts[1]

	data, err := base64.StdEncoding.DecodeString(b64data)
	if err != nil {
		http.Error(w, "invalid base64", http.StatusBadRequest)
		return
	}

	// Compress + dedup (reuse logic from image_cache.go).
	finalData, finalMime, _ := compressForStorage(data, mime)
	hash := sha256Hex(finalData)

	if existingID, hit := hashIndex.Load(hash); hit {
		id := existingID.(string)
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(map[string]any{"image_id": id, "mime": finalMime}) //nolint:errcheck
		return
	}

	id := lfUUID()
	if minioEnabled() {
		if err := minioPut(id, finalMime, finalData); err != nil {
			http.Error(w, "storage error: "+err.Error(), http.StatusInternalServerError)
			return
		}
		imageCache.Store(id, imageEntry{Mime: finalMime})
	} else {
		imageCache.Store(id, imageEntry{
			Mime: finalMime,
			B64:  base64.StdEncoding.EncodeToString(finalData),
		})
	}
	hashIndex.Store(hash, id)

	fmt.Printf("[upload-image] stored %s (%s, %s)\n", id, finalMime, fmtSize(len(finalData)))
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(map[string]any{"image_id": id, "mime": finalMime}) //nolint:errcheck
}

// ── Artifact endpoint ─────────────────────────────────────────────────────────

func artifactHandler(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	id := strings.TrimPrefix(r.URL.Path, "/artifact/")
	if id == "" {
		http.Error(w, "missing id", http.StatusBadRequest)
		return
	}
	art := getArtifactByID(id)
	if art == nil {
		http.Error(w, "not found", http.StatusNotFound)
		return
	}
	download := r.URL.Query().Get("download") == "1"
	if download {
		ascii := strings.Map(func(r rune) rune {
			if r >= 0x20 && r <= 0x7e {
				return r
			}
			return '_'
		}, art.Filename)
		w.Header().Set("Content-Disposition", fmt.Sprintf(`attachment; filename="%s"`, ascii))
	}
	w.Header().Set("Content-Type", art.MimeType)
	w.Header().Set("Content-Length", fmt.Sprintf("%d", len(art.Content)))
	w.Write(art.Content) //nolint:errcheck
}

// ── Upload-file (non-image files: xlsx, csv, json, …) ────────────────────────

var blockedUploadExts = map[string]bool{
	"exe": true, "bat": true, "cmd": true, "com": true, "msi": true,
	"dll": true, "scr": true, "ps1": true, "vbs": true, "vbe": true,
	"ws": true, "wsf": true, "pif": true, "jar": true, "app": true,
	"deb": true, "rpm": true, "dmg": true, "pkg": true,
}

const maxUploadBytes = 25 << 20 // 25 MiB — matches Python lab

type uploadFileReq struct {
	Filename  string `json:"filename"`
	DataB64   string `json:"data_b64"`
	MimeType  string `json:"mime_type"`
	SessionID string `json:"session_id"`
}

func uploadFileHandler(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	r.Body = http.MaxBytesReader(w, r.Body, maxUploadBytes+1024) // slight headroom for JSON envelope

	var req uploadFileReq
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		http.Error(w, "bad request", http.StatusBadRequest)
		return
	}
	if req.Filename == "" || req.SessionID == "" || req.DataB64 == "" {
		http.Error(w, "filename, session_id and data_b64 required", http.StatusBadRequest)
		return
	}

	// Sanitize filename: strip path separators
	safe := filepath.Base(req.Filename)
	if safe == "." || safe == "/" {
		http.Error(w, "invalid filename", http.StatusBadRequest)
		return
	}

	// Block dangerous extensions (server-side — mirrors frontend + Python lab)
	ext := strings.ToLower(strings.TrimPrefix(filepath.Ext(safe), "."))
	if blockedUploadExts[ext] {
		http.Error(w, fmt.Sprintf("file type .%s is not allowed", ext), http.StatusBadRequest)
		return
	}

	data, err := base64.StdEncoding.DecodeString(req.DataB64)
	if err != nil {
		http.Error(w, "invalid base64", http.StatusBadRequest)
		return
	}
	if len(data) > maxUploadBytes {
		http.Error(w, "file too large (max 25 MB)", http.StatusRequestEntityTooLarge)
		return
	}

	dir := sessionUploadDir(req.SessionID)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		http.Error(w, "storage error", http.StatusInternalServerError)
		return
	}

	dest := filepath.Join(dir, safe)
	if err := os.WriteFile(dest, data, 0o644); err != nil {
		http.Error(w, "write error: "+err.Error(), http.StatusInternalServerError)
		return
	}

	fmt.Printf("[upload-file] session=%s file=%s size=%s\n", req.SessionID[:8], safe, fmtSize(len(data)))

	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(map[string]any{ //nolint:errcheck
		"ok":       true,
		"filename": safe,
		"size":     len(data),
		"path":     "/uploaded/" + safe,
	})
}

// ── Config endpoint ───────────────────────────────────────────────────────────

func configHandler(w http.ResponseWriter, _ *http.Request) {
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Cache-Control", "no-store")
	json.NewEncoder(w).Encode(map[string]any{ //nolint:errcheck
		"root_agent": "deepseek/" + _agentModel,
		"search":     "gemini/" + _searchModel,
		"code_exec":  "disabled",
		"skills":     listSkills(),
	})
}

// ── Static files ──────────────────────────────────────────────────────────────

func staticHandler() http.Handler {
	candidates := []string{
		"static",
		"examples/single_agent_harness/static",
		"single_agent_harness/static",
	}
	for _, p := range candidates {
		if info, err := os.Stat(p); err == nil && info.IsDir() {
			fmt.Printf("[single_agent_harness] serving static from %s\n", p)
			fs := http.FileServer(http.Dir(p))
			return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.URL.Path == "/" || r.URL.Path == "/index.html" {
					w.Header().Set("Cache-Control", "no-store, no-cache, must-revalidate")
					w.Header().Set("Pragma", "no-cache")
				}
				fs.ServeHTTP(w, r)
			})
		}
	}
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprintln(w, "Single Agent Harness API running.")
		fmt.Fprintln(w, "Place index.html in static/ directory.")
	})
}

// ── Middleware ────────────────────────────────────────────────────────────────

func corsMiddleware(next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Access-Control-Allow-Origin", "*")
		w.Header().Set("Access-Control-Allow-Headers", "Content-Type")
		w.Header().Set("Access-Control-Allow-Methods", "GET, POST, DELETE, PATCH, OPTIONS")
		if r.Method == http.MethodOptions {
			w.WriteHeader(http.StatusNoContent)
			return
		}
		next(w, r)
	}
}

// ── Session REST endpoints ─────────────────────────────────────────────────────

func sessionsListHandler(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	fmt.Printf("[http] GET /sessions\n")
	sessions, err := listSessions(50)
	if err != nil {
		fmt.Printf("[http] GET /sessions error: %v\n", err)
		http.Error(w, "db error: "+err.Error(), http.StatusInternalServerError)
		return
	}
	if sessions == nil {
		sessions = []SessionPreview{}
	}
	fmt.Printf("[http] GET /sessions → %d items\n", len(sessions))
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(sessions) //nolint:errcheck
}

func sessionDetailHandler(w http.ResponseWriter, r *http.Request) {
	path := strings.TrimPrefix(r.URL.Path, "/sessions/")
	parts := strings.SplitN(path, "/", 2)
	sessionID := parts[0]
	fmt.Printf("[http] %s /sessions/%s\n", r.Method, sessionID[:min(8, len(sessionID))])
	if sessionID == "" {
		http.Error(w, "missing session id", http.StatusBadRequest)
		return
	}

	switch {
	case r.Method == http.MethodGet && len(parts) == 1:
		// GET /sessions/{id}
		doc, err := getSessionDoc(sessionID)
		if err != nil || doc == nil {
			http.Error(w, "not found", http.StatusNotFound)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(map[string]any{ //nolint:errcheck
			"session_id":       doc.SessionID,
			"name":             doc.Name,
			"status":           doc.Status,
			"last_stop_reason": doc.LastStopReason,
			"interrupted":      doc.Interrupted,
			"updated_at":       doc.UpdatedAt,
			"ui_messages":      doc.UIMessages,
			"ds_messages":      doc.DSMessages,
		})

	case r.Method == http.MethodDelete && len(parts) == 1:
		// DELETE /sessions/{id}
		if err := deleteSessionDoc(sessionID); err != nil {
			http.Error(w, "db error: "+err.Error(), http.StatusInternalServerError)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(map[string]any{"ok": true}) //nolint:errcheck

	case r.Method == http.MethodPatch && len(parts) == 2 && parts[1] == "name":
		// PATCH /sessions/{id}/name
		var body struct {
			Name string `json:"name"`
		}
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil || body.Name == "" {
			http.Error(w, "bad request: name required", http.StatusBadRequest)
			return
		}
		if err := renameSessionDoc(sessionID, body.Name); err != nil {
			http.Error(w, "db error: "+err.Error(), http.StatusInternalServerError)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(map[string]any{"ok": true}) //nolint:errcheck

	default:
		http.Error(w, "not found", http.StatusNotFound)
	}
}

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
	cwd, _ := os.Getwd()

	// 1. cwd/.env (highest priority — overrides everything below)
	loadDotEnvFile(filepath.Join(cwd, ".env"))

	// 2. Exe's own directory (compiled binary next to its .env)
	if exe, err := os.Executable(); err == nil {
		exeDir := filepath.Dir(exe)
		loadDotEnvFile(filepath.Join(exeDir, ".env"))

		// 3. cwd/<binary-name>/.env — handles "go run ./single_agent_harness/" from examples/
		//    Go names the temp exe after the package directory, so this resolves correctly.
		binName := strings.TrimSuffix(filepath.Base(exe), ".exe")
		loadDotEnvFile(filepath.Join(cwd, binName, ".env"))
	}

	// 4. Walk up parent directories
	dir := cwd
	for i := 0; i < 4; i++ {
		parent := filepath.Dir(dir)
		if parent == dir {
			break
		}
		dir = parent
		loadDotEnvFile(filepath.Join(dir, ".env"))
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

func getEnv(key, defaultVal string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return defaultVal
}

// ── SSE helper ────────────────────────────────────────────────────────────────

func writeSSE(w http.ResponseWriter, evType string, data any) {
	fmt.Fprint(w, SSEEvent{Type: evType, Data: data}.Encode())
}

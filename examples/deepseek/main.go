// DeepSeek HTTP Spy (Go) — raw net/http, no SDK.
// Direct port of deepseek_spy.py with added resource-usage benchmarking.
//
// Run from repo root:
//
//	go run ./examples/deepseek                  # non-stream + stream, thinking disabled
//	go run ./examples/deepseek thinking         # thinking ON vs OFF comparison
//	go run ./examples/deepseek tool_calls       # streaming tool-call delta analysis (T0/T1/T2)
//	go run ./examples/deepseek load [N]         # N concurrent streams + resource stats (default 8)
package main

import (
	"bufio"
	"bytes"
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"runtime"
	"strconv"
	"strings"
	"sync"
	"time"
)

const (
	apiURL = "https://api.deepseek.com/chat/completions"
	lineW  = 72
)

var (
	apiKey     string
	httpClient = &http.Client{} // shared, no global timeout
)

// ── pretty-print helpers ──────────────────────────────────────────────────────

func hdr(label string) {
	bar := strings.Repeat("=", lineW)
	fmt.Printf("\n%s\n  %s\n%s\n", bar, label, bar)
}

func section(label string) {
	bar := strings.Repeat("-", lineW)
	fmt.Printf("\n%s\n  %s\n%s\n", bar, label, bar)
}

func pJSON(v any) {
	b, err := json.MarshalIndent(v, "    ", "  ")
	if err != nil {
		fmt.Printf("    %v\n", v)
		return
	}
	fmt.Printf("    %s\n", b)
}

func fmtJSON(v any) string {
	b, _ := json.MarshalIndent(v, "", "  ")
	return string(b)
}

func fmtMB(b uint64) string { return fmt.Sprintf("%.2f MB", float64(b)/1024/1024) }

func redactedKey() string {
	if len(apiKey) < 10 {
		return apiKey
	}
	return apiKey[:10] + "...[redacted]"
}

// ── HTTP helpers ──────────────────────────────────────────────────────────────

func doRequest(payload map[string]any, stream bool) (*http.Response, error) {
	b, err := json.Marshal(payload)
	if err != nil {
		return nil, err
	}
	req, err := http.NewRequest(http.MethodPost, apiURL, bytes.NewReader(b))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Authorization", "Bearer "+apiKey)
	req.Header.Set("Content-Type", "application/json")
	if stream {
		req.Header.Set("Accept", "text/event-stream")
	}
	return httpClient.Do(req)
}

func copyMap(m map[string]any) map[string]any {
	out := make(map[string]any, len(m))
	for k, v := range m {
		out[k] = v
	}
	return out
}

// ── resource stats ────────────────────────────────────────────────────────────

type memSnap struct {
	label      string
	goroutines int
	heapAlloc  uint64
	heapSys    uint64
	stackInuse uint64
	numGC      uint32
}

func takeSnap(label string) memSnap {
	var ms runtime.MemStats
	runtime.ReadMemStats(&ms)
	return memSnap{
		label:      label,
		goroutines: runtime.NumGoroutine(),
		heapAlloc:  ms.HeapAlloc,
		heapSys:    ms.HeapSys,
		stackInuse: ms.StackInuse,
		numGC:      ms.NumGC,
	}
}

func (s memSnap) print() {
	fmt.Printf("  [%-12s]  goroutines=%-4d  heap_alloc=%-10s  heap_sys=%-10s  stack=%s  gc=%d\n",
		s.label, s.goroutines, fmtMB(s.heapAlloc), fmtMB(s.heapSys), fmtMB(s.stackInuse), s.numGC)
}

// ── spy: non-streaming ────────────────────────────────────────────────────────

func spyNonStream(payload map[string]any) {
	hdr("NON-STREAMING CALL")
	section("→ REQUEST")
	fmt.Printf("  POST %s\n", apiURL)
	fmt.Printf("  Authorization: Bearer %s\n", redactedKey())
	fmt.Printf("  Content-Type: application/json\n\n  BODY:\n")
	pJSON(payload)

	t0 := time.Now()
	resp, err := doRequest(payload, false)
	elapsed := time.Since(t0)
	if err != nil {
		fmt.Printf("[ERR] %v\n", err)
		return
	}
	defer resp.Body.Close()

	section(fmt.Sprintf("← RESPONSE  HTTP %d  [%.3fs]", resp.StatusCode, elapsed.Seconds()))
	for k, vs := range resp.Header {
		fmt.Printf("  %s: %s\n", k, strings.Join(vs, ", "))
	}

	var body map[string]any
	if err := json.NewDecoder(resp.Body).Decode(&body); err != nil {
		fmt.Printf("[ERR] decode: %v\n", err)
		return
	}
	fmt.Printf("\n  BODY:\n")
	pJSON(body)

	choices, _ := body["choices"].([]any)
	if len(choices) == 0 {
		return
	}
	msg, _ := choices[0].(map[string]any)["message"].(map[string]any)
	content, _ := msg["content"].(string)
	reasoning, _ := msg["reasoning_content"].(string)
	var compTok float64
	if usage, ok := body["usage"].(map[string]any); ok {
		compTok, _ = usage["completion_tokens"].(float64)
	}
	tps := compTok / elapsed.Seconds()
	fmt.Printf("\n  TIMING  total=%.3fs  |  %.0f tokens  |  %.1f tok/s\n", elapsed.Seconds(), compTok, tps)
	if reasoning != "" {
		fmt.Printf("  !! [thinking ON]  reasoning_content PRESENT (%d chars)\n", len(reasoning))
	} else {
		fmt.Printf("  [OK] No reasoning_content — thinking is OFF\n")
	}
	fmt.Printf("\n  [OK] Answer: %s\n", content)
}

// ── spy: streaming SSE ────────────────────────────────────────────────────────

func spyStream(payload map[string]any) {
	hdr("STREAMING CALL (SSE)")
	p := copyMap(payload)
	p["stream"] = true

	section("→ REQUEST")
	fmt.Printf("  POST %s\n", apiURL)
	fmt.Printf("  Authorization: Bearer %s\n", redactedKey())
	fmt.Printf("  Content-Type: application/json\n  Accept: text/event-stream\n\n  BODY:\n")
	pJSON(p)

	t0 := time.Now()
	resp, err := doRequest(p, true)
	if err != nil {
		fmt.Printf("[ERR] %v\n", err)
		return
	}
	defer resp.Body.Close()

	section("<- RESPONSE  (SSE events)")
	fmt.Printf("  HTTP %d\n", resp.StatusCode)
	for k, vs := range resp.Header {
		fmt.Printf("  %s: %s\n", k, strings.Join(vs, ", "))
	}
	fmt.Printf("\n  SSE STREAM (each event pretty-printed):\n\n")

	var (
		content    strings.Builder
		reasoning  strings.Builder
		chunks     []string
		eventCount int
		compTokens float64
		tFirst     time.Time
		gotFirst   bool
	)

	scanner := bufio.NewScanner(resp.Body)
	scanner.Buffer(make([]byte, 256*1024), 256*1024)
	for scanner.Scan() {
		line := scanner.Text()
		if line == "" {
			continue
		}
		if line == "data: [DONE]" {
			fmt.Printf("  data: [DONE]\n")
			continue
		}
		if !strings.HasPrefix(line, "data: ") {
			fmt.Printf("  %s\n", line)
			continue
		}

		var data map[string]any
		if err := json.Unmarshal([]byte(line[6:]), &data); err != nil {
			fmt.Printf("  %s\n", line)
			continue
		}

		elapsedMs := float64(time.Since(t0).Milliseconds())
		eventCount++

		choices, _ := data["choices"].([]any)
		if len(choices) > 0 {
			delta, _ := choices[0].(map[string]any)["delta"].(map[string]any)
			tok, _ := delta["content"].(string)
			rsn, _ := delta["reasoning_content"].(string)
			content.WriteString(tok)
			reasoning.WriteString(rsn)
			if tok != "" {
				if !gotFirst {
					gotFirst = true
					tFirst = time.Now()
				}
				chunks = append(chunks, tok)
			}
		}
		if usage, ok := data["usage"].(map[string]any); ok {
			if ct, ok := usage["completion_tokens"].(float64); ok {
				compTokens = ct
			}
		}

		pretty := fmtJSON(data)
		fmt.Printf("  -- event #%d  +%.0fms %s\n", eventCount, elapsedMs, strings.Repeat("-", 30))
		for _, pline := range strings.Split(pretty, "\n") {
			marker := "     "
			if strings.Contains(pline, `"delta"`) || strings.Contains(pline, `"content"`) {
				marker = "  >>  "
			}
			fmt.Printf("%s%s\n", marker, pline)
		}
	}

	tEnd := time.Now()
	total := tEnd.Sub(t0)
	var ttft, genTime time.Duration
	if gotFirst {
		ttft = tFirst.Sub(t0)
		genTime = tEnd.Sub(tFirst)
	} else {
		genTime = total
	}
	nTok := compTokens
	if nTok == 0 {
		nTok = float64(len(chunks))
	}
	tps := 0.0
	if genTime > 0 {
		tps = nTok / genTime.Seconds()
	}

	bar := strings.Repeat("=", 50)
	fmt.Printf("\n  %s\n  TIMING SUMMARY\n  %s\n", bar, bar)
	if gotFirst {
		fmt.Printf("  Time to first token (TTFT) : %d ms\n", ttft.Milliseconds())
	} else {
		fmt.Printf("  TTFT: n/a (no content tokens — thinking may be ON)\n")
	}
	fmt.Printf("  Total time                 : %.3f s\n", total.Seconds())
	fmt.Printf("  Generation time            : %.3f s  (after first token)\n", genTime.Seconds())
	fmt.Printf("  Completion tokens          : %.0f tok\n", nTok)
	fmt.Printf("  Speed                      : %.1f tok/s\n", tps)
	fmt.Printf("  %s\n", bar)
	fmt.Printf("\n  TOKEN CHUNKS: %v\n", chunks)
	fmt.Printf("  ASSEMBLED   : %q\n", content.String())
	if reasoning.Len() > 0 {
		fmt.Printf("\n  !! [thinking ON]  reasoning_content streamed (%d chars)\n", reasoning.Len())
	} else {
		fmt.Printf("\n  [OK] No reasoning_content — thinking is OFF\n")
	}
}

// ── demo: tool call streaming ─────────────────────────────────────────────────

func demoToolCalls() {
	hdr("TOOL CALL STREAMING SPY")

	tools := []map[string]any{{
		"type": "function",
		"function": map[string]any{
			"name":        "web_search",
			"description": "Search the web for current information.",
			"parameters": map[string]any{
				"type": "object",
				"properties": map[string]any{
					"query": map[string]any{
						"type":        "string",
						"description": "Search query",
					},
				},
				"required": []string{"query"},
			},
		},
	}}

	payload := map[string]any{
		"model":          "deepseek-v4-flash",
		"max_tokens":     256,
		"thinking":       map[string]any{"type": "disabled"},
		"temperature":    0,
		"stream":         true,
		"stream_options": map[string]any{"include_usage": true},
		"tools":          tools,
		"tool_choice":    "required",
		"messages": []map[string]any{{
			"role":    "user",
			"content": "Tìm kiếm thông tin về Claude 4 Sonnet của Anthropic ra mắt năm 2025",
		}},
	}

	section("→ REQUEST (tool_choice=required forces tool call)")
	pJSON(payload)

	t0 := time.Now()
	resp, err := doRequest(payload, true)
	if err != nil {
		fmt.Printf("[ERR] %v\n", err)
		return
	}
	defer resp.Body.Close()

	section("<- RESPONSE  (SSE events — focus on tool_calls deltas)")
	fmt.Printf("  HTTP %d\n\n", resp.StatusCode)

	// Per-index tool-call state
	type tcState struct {
		name       string
		id         string
		args       strings.Builder
		tName      float64 // ms since t0 when name first seen (-1 = not seen)
		tArgsFirst float64
		tArgsLast  float64
		nChunks    int
	}
	tcs := map[int]*tcState{}
	getTC := func(idx int) *tcState {
		if tcs[idx] == nil {
			tcs[idx] = &tcState{tName: -1, tArgsFirst: -1, tArgsLast: -1}
		}
		return tcs[idx]
	}

	eventCount := 0
	scanner := bufio.NewScanner(resp.Body)
	scanner.Buffer(make([]byte, 256*1024), 256*1024)
	for scanner.Scan() {
		line := scanner.Text()
		if line == "" {
			continue
		}
		if line == "data: [DONE]" {
			fmt.Printf("\n  data: [DONE]\n")
			continue
		}
		if !strings.HasPrefix(line, "data: ") {
			continue
		}

		var data map[string]any
		if err := json.Unmarshal([]byte(line[6:]), &data); err != nil {
			fmt.Printf("  %s\n", line)
			continue
		}

		elapsedMs := float64(time.Since(t0).Milliseconds())
		eventCount++

		// usage-only chunk (no choices)
		choices, _ := data["choices"].([]any)
		if len(choices) == 0 {
			if usage, ok := data["usage"].(map[string]any); ok {
				fmt.Printf("  -- event #%d  +%.0fms  [usage] prompt=%.0f completion=%.0f\n",
					eventCount, elapsedMs,
					usage["prompt_tokens"], usage["completion_tokens"])
			}
			continue
		}

		choice := choices[0].(map[string]any)
		delta, _ := choice["delta"].(map[string]any)
		finish, _ := choice["finish_reason"].(string)
		contentStr, _ := delta["content"].(string)
		tcDeltas, _ := delta["tool_calls"].([]any)

		if contentStr != "" {
			fmt.Printf("  -- event #%d  +%.0fms  [content preamble]  %q\n", eventCount, elapsedMs, contentStr)
		}

		for _, tci := range tcDeltas {
			tc, _ := tci.(map[string]any)
			idx := 0
			if idxF, ok := tc["index"].(float64); ok {
				idx = int(idxF)
			}
			tid, _ := tc["id"].(string)
			fn, _ := tc["function"].(map[string]any)
			tcName, _ := fn["name"].(string)
			tcArgs, _ := fn["arguments"].(string)

			st := getTC(idx)
			if tid != "" && st.id == "" {
				st.id = tid
			}
			if tcName != "" {
				if st.tName < 0 {
					st.tName = elapsedMs
				}
				st.name += tcName
			}
			if tcArgs != "" {
				if st.tArgsFirst < 0 {
					st.tArgsFirst = elapsedMs
				}
				st.tArgsLast = elapsedMs
				st.args.WriteString(tcArgs)
				st.nChunks++
			}

			var idPart, namePart, argsPart string
			if tid != "" {
				idPart = fmt.Sprintf(" id=%q", tid)
			}
			if tcName != "" {
				namePart = fmt.Sprintf(" name=%q", tcName)
			}
			if tcArgs != "" {
				argsPart = fmt.Sprintf(" args_chunk=%q", tcArgs)
			}
			fmt.Printf("  -- event #%d  +%.0fms  [tool_calls idx=%d]%s%s%s\n",
				eventCount, elapsedMs, idx, idPart, namePart, argsPart)
		}

		if finish != "" {
			fmt.Printf("  -- event #%d  +%.0fms  finish_reason=%q\n", eventCount, elapsedMs, finish)
		}
	}

	// ── summary ────────────────────────────────────────────────────────────────
	section("TOOL CALL STREAM ANALYSIS")
	for idx := 0; idx < len(tcs); idx++ {
		st, ok := tcs[idx]
		if !ok {
			continue
		}
		fmt.Printf("\n  Tool [%d]  name=%q  id=%q\n", idx, st.name, st.id)
		if st.tName >= 0 {
			fmt.Printf("    ▸ function.name first seen : +%.0fms\n", st.tName)
		}
		if st.tArgsFirst >= 0 {
			fmt.Printf("    ▸ arguments first chunk    : +%.0fms  (gap from name: %.0fms)\n",
				st.tArgsFirst, st.tArgsFirst-st.tName)
		}
		if st.tArgsLast >= 0 && st.tArgsFirst >= 0 {
			fmt.Printf("    ▸ arguments last chunk     : +%.0fms  (arg span: %.0fms,  %d chunks)\n",
				st.tArgsLast, st.tArgsLast-st.tArgsFirst, st.nChunks)
		}
		fmt.Printf("    ▸ assembled arguments      : %s\n\n", st.args.String())

		fmt.Printf("  VERDICT:\n")
		switch {
		case st.nChunks > 1:
			fmt.Printf("    ✅ Arguments streamed incrementally — %d chunks over %.0fms\n",
				st.nChunks, st.tArgsLast-st.tArgsFirst)
			fmt.Printf("    → Early chip: emit tool_call on first name delta (+%.0fms)\n", st.tName)
			fmt.Printf("      stream arg chunks → FE tool_arg_stream events\n")
			fmt.Printf("      saving ~%.0fms of chip latency vs waiting for [DONE]\n", st.tArgsLast)
		case st.nChunks == 1:
			fmt.Printf("    ⚠️  Arguments arrived as 1 single chunk (no incremental streaming)\n")
			fmt.Printf("    → Early chip still possible: emit on name delta, %.0fms earlier\n",
				st.tArgsFirst-st.tName)
		default:
			fmt.Printf("    ❓ No argument chunks captured — unexpected\n")
		}
	}
}

// ── demo: concurrent load + resource comparison ───────────────────────────────

func demoLoad(n int) {
	hdr(fmt.Sprintf("RESOURCE USAGE — %d CONCURRENT STREAMING REQUESTS", n))

	// GC once to get a clean baseline
	runtime.GC()
	snapIdle := takeSnap("idle")
	snapIdle.print()
	fmt.Println()

	payload := map[string]any{
		"model":      "deepseek-v4-flash",
		"max_tokens": 64,
		"thinking":   map[string]any{"type": "disabled"},
		"stream":     true,
		"messages": []map[string]any{{
			"role":    "user",
			"content": "Write a haiku about Go concurrency.",
		}},
	}

	// background peak sampler (10ms polling)
	var (
		peakGoroutines int
		peakHeapAlloc  uint64
		peakMu         sync.Mutex
		samplerDone    = make(chan struct{})
	)
	go func() {
		var ms runtime.MemStats
		ticker := time.NewTicker(10 * time.Millisecond)
		defer ticker.Stop()
		for {
			select {
			case <-samplerDone:
				return
			case <-ticker.C:
				runtime.ReadMemStats(&ms)
				g := runtime.NumGoroutine()
				peakMu.Lock()
				if g > peakGoroutines {
					peakGoroutines = g
				}
				if ms.HeapAlloc > peakHeapAlloc {
					peakHeapAlloc = ms.HeapAlloc
				}
				peakMu.Unlock()
			}
		}
	}()

	// launch N concurrent workers
	var (
		wg          sync.WaitGroup
		mu          sync.Mutex
		totalTokens int
		results     []string
	)
	t0 := time.Now()
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func(id int) {
			defer wg.Done()
			tReq := time.Now()
			resp, err := doRequest(copyMap(payload), true)
			if err != nil {
				fmt.Printf("  [worker %02d] ERR: %v\n", id, err)
				return
			}
			defer resp.Body.Close()

			var toks int
			scanner := bufio.NewScanner(resp.Body)
			scanner.Buffer(make([]byte, 64*1024), 64*1024)
			for scanner.Scan() {
				line := scanner.Text()
				if line == "" || line == "data: [DONE]" {
					continue
				}
				if strings.HasPrefix(line, "data: ") {
					var data map[string]any
					if json.Unmarshal([]byte(line[6:]), &data) == nil {
						if usage, ok := data["usage"].(map[string]any); ok {
							if ct, ok := usage["completion_tokens"].(float64); ok {
								toks = int(ct)
							}
						}
					}
				}
			}
			elapsed := time.Since(tReq)
			mu.Lock()
			totalTokens += toks
			results = append(results, fmt.Sprintf("  [worker %02d]  tokens=%-4d  time=%.2fs", id, toks, elapsed.Seconds()))
			mu.Unlock()
		}(i)
	}
	wg.Wait()
	close(samplerDone)
	wallTime := time.Since(t0)

	for _, r := range results {
		fmt.Println(r)
	}

	peakMu.Lock()
	pg := peakGoroutines
	ph := peakHeapAlloc
	peakMu.Unlock()

	runtime.GC()
	snapAfter := takeSnap("after-load")

	section("RESULTS")
	fmt.Printf("  Workers (goroutines)  : %d\n", n)
	fmt.Printf("  Total tokens          : %d\n", totalTokens)
	fmt.Printf("  Wall time             : %.3fs\n", wallTime.Seconds())
	fmt.Printf("  Throughput            : %.1f tok/s  (aggregate)\n\n", float64(totalTokens)/wallTime.Seconds())

	fmt.Printf("  %-22s goroutines=%-4d  heap=%s\n", "idle:", snapIdle.goroutines, fmtMB(snapIdle.heapAlloc))
	fmt.Printf("  %-22s goroutines=%-4d  heap=%s\n", "peak (sampled):", pg, fmtMB(ph))
	fmt.Printf("  %-22s goroutines=%-4d  heap=%s\n", "after-load (post-GC):", snapAfter.goroutines, fmtMB(snapAfter.heapAlloc))
	fmt.Printf("\n  heap delta idle→peak  : +%s\n", fmtMB(ph-snapIdle.heapAlloc))

	section("PYTHON BASELINE (run manually to compare)")
	fmt.Printf("  # Install:  pip install httpx psutil\n")
	fmt.Printf("  # Measure:  python deepseek/deepseek_spy.py\n")
	fmt.Printf("  # Under load (N threads): use threading.Thread + the spy_stream() call\n\n")
	fmt.Printf("  Typical Python idle RSS      : 25–45 MB  (interpreter + httpx + dotenv + GIL)\n")
	fmt.Printf("  Go idle heap (this run)      : %s\n", fmtMB(snapIdle.heapAlloc))
	fmt.Printf("  Per-goroutine stack          : ~2–8 KB  (vs ~1 MB Python thread default)\n")
	fmt.Printf("  For %d concurrent in Python  : ~%d MB threads  vs ~%.1f MB goroutine stacks (est.)\n",
		n, n*1, float64(n*6*1024)/1024/1024)
}

// ── demo scenarios ────────────────────────────────────────────────────────────

const benchPrompt = "Explain what LangGraph is and why it's useful for building AI agents. Answer in 3-4 sentences."

var basePayload = map[string]any{
	"model":      "deepseek-v4-flash",
	"max_tokens": 256,
	"messages":   []map[string]any{{"role": "user", "content": benchPrompt}},
}

func demoDefault() {
	p := copyMap(basePayload)
	p["thinking"] = map[string]any{"type": "disabled"}
	spyNonStream(p)
	spyStream(p)
}

func demoThinkingComparison() {
	fmt.Printf("\n[>>] THINKING ON  (default — no 'thinking' field in payload)\n")
	spyNonStream(copyMap(basePayload))
	fmt.Printf("\n\n[>>] THINKING OFF (thinking.type=disabled)\n")
	p := copyMap(basePayload)
	p["thinking"] = map[string]any{"type": "disabled"}
	spyNonStream(p)
}

// ── main ──────────────────────────────────────────────────────────────────────

func main() {
	apiKey = os.Getenv("DEEPSEEK_API_KEY")
	if apiKey == "" {
		fmt.Fprintln(os.Stderr, "[ERR] DEEPSEEK_API_KEY not set")
		os.Exit(1)
	}

	mode := "default"
	if len(os.Args) > 1 {
		mode = os.Args[1]
	}
	n := 8
	if len(os.Args) > 2 {
		if v, err := strconv.Atoi(os.Args[2]); err == nil && v > 0 {
			n = v
		}
	}

	switch mode {
	case "thinking":
		demoThinkingComparison()
	case "tool_calls":
		demoToolCalls()
	case "load":
		demoLoad(n)
	default:
		demoDefault()
	}
}

package main

import (
	"fmt"
	"os"
	"strings"
	"time"
)

// vnLoc is Vietnam Standard Time (UTC+7).
// Fixed zone — no tzdata package needed in minimal Linux containers.
var vnLoc = time.FixedZone("ICT", 7*3600)

// agentLogEnabled is set once at startup from the LOG_AGENT env var.
var agentLogEnabled bool

func initAgentLog() {
	v := strings.ToLower(strings.TrimSpace(os.Getenv("LOG_AGENT")))
	agentLogEnabled = v == "true" || v == "1" || v == "yes"
}

// roundLog holds timing + context for one LLM streaming round.
type roundLog struct {
	SessionID   string
	Round       int
	UserMsg     string  // latest user message in the conversation turn
	GatewayURL  string  // full API endpoint URL
	ConnectMS   float64 // time from HTTP request start → response headers received (ms)
	TTFT_MS     float64 // time from HTTP request start → first content or tool-call token (ms)
	GenMS       float64 // total streaming duration: request start → stream EOF (ms)
	PromptTok   int
	CompleteTok int
	HasTools    bool // true when response was tool calls, false when text
}

// logRound prints one structured log line per LLM round and emits a Kafka event.
// The fmt.Printf is a no-op when LOG_AGENT is off; emitEvent always runs.
func logRound(l roundLog) {
	kind := "text"
	if l.HasTools {
		kind = "tools"
	}
	tokPerSec := 0.0
	if l.GenMS > 0 {
		tokPerSec = float64(l.CompleteTok) / (l.GenMS / 1000.0)
	}

	// Kafka event — always emit regardless of LOG_AGENT flag.
	userMsgShort := l.UserMsg
	if runes := []rune(l.UserMsg); len(runes) > 200 {
		userMsgShort = string(runes[:200])
	}
	emitEvent(TurnEvent{
		EventType:    "agent.turn",
		Ts:           time.Now().UTC().Format(time.RFC3339),
		SessionID:    l.SessionID,
		Round:        l.Round,
		Model:        _agentModel,
		Gateway:      gatewayHost(l.GatewayURL),
		ConnectMS:    l.ConnectMS,
		TTFT_MS:      l.TTFT_MS,
		GenMS:        l.GenMS,
		PromptTok:    l.PromptTok,
		CompleteTok:  l.CompleteTok,
		TokPerSec:    tokPerSec,
		ResponseType: kind,
		UserMsg:      userMsgShort,
	})

	if !agentLogEnabled {
		return
	}
	now := time.Now().In(vnLoc).Format("2006-01-02 15:04:05")
	// Truncate to 60 runes (safe for Vietnamese multi-byte chars).
	runes := []rune(l.UserMsg)
	msg := string(runes)
	if len(runes) > 60 {
		msg = string(runes[:57]) + "..."
	}
	fmt.Printf(
		"[turn] %s ICT  session=%s round=%d  gw=%s  connect=%s ttft=%s gen=%s tok=+%d/%d(%.0ft/s) →%s  %q\n",
		now, l.SessionID[:8], l.Round,
		gatewayHost(l.GatewayURL),
		fmtDurMS(l.ConnectMS), fmtDurMS(l.TTFT_MS), fmtDurMS(l.GenMS),
		l.PromptTok, l.CompleteTok, tokPerSec, kind,
		msg,
	)
}

// fmtDurMS formats a millisecond value as "Xms" (< 1 s) or "X.Xs" (≥ 1 s).
func fmtDurMS(ms float64) string {
	if ms >= 1000 {
		return fmt.Sprintf("%.1fs", ms/1000)
	}
	return fmt.Sprintf("%.0fms", ms)
}

// gatewayHost strips protocol and path from a URL, returning just the host.
// "https://api.deepseek.com/chat/completions" → "api.deepseek.com"
func gatewayHost(rawURL string) string {
	s := strings.TrimPrefix(rawURL, "https://")
	s = strings.TrimPrefix(s, "http://")
	if i := strings.Index(s, "/"); i >= 0 {
		s = s[:i]
	}
	return s
}

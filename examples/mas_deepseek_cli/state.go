package main

import "time"

// Message represents a single conversation message.
type Message struct {
	Role    string // "user" | "model"
	Content string
	Name    string // agent name (e.g. "code_expert"), empty for user messages
}

// AgentState holds all state flowing through the langgraphgo graph.
type AgentState struct {
	Messages   []Message
	Next       string
	CallCount  map[string]int
	Records    []StepRecord
	UserMsgIdx int
	Step       int
}

// Router is the JSON schema for the supervisor's routing decision.
type Router struct {
	Reasoning string `json:"reasoning"`
	Next      string `json:"next"`
}

// StepRecord stores per-step timing and token metrics.
type StepRecord struct {
	Step    int
	Agent   string
	Elapsed time.Duration
	Tokens  int           // completion tokens (0 for non-streaming routing calls)
	TTFT    time.Duration // time from request start to first content token (includes network)
	GenTime time.Duration // time from first token to stream end
}

// agentNodes is the set of specialist agent node names.
// No web_search: DeepSeek has no built-in grounding tool like Gemini's googleSearch.
var agentNodes = map[string]bool{
	"code_expert":     true,
	"data_analyst":    true,
	"security_expert": true,
	"writing_expert":  true,
	"devops_expert":   true,
}

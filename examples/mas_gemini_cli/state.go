package main

import "time"

// Message represents a single conversation message.
type Message struct {
	Role    string // "user" | "model"
	Content string
	Name    string // agent name (e.g. "code_expert"), empty for user messages
}

// AgentState holds all state flowing through the langgraphgo graph.
// Each field is carried forward by every node; the default "last-write-wins"
// merge is intentional — we run sequentially and return full state from each node.
type AgentState struct {
	Messages   []Message      // full conversation history (append-only in practice)
	Next       string         // routing target set by supervisor node
	CallCount  map[string]int // tracks how many times each specialist ran (anti-loop)
	Records    []StepRecord   // timing records accumulated across the turn
	UserMsgIdx int            // index of the latest user message in Messages
	Step       int            // step counter, incremented by supervisor each call
}

// Router is the JSON schema for the supervisor's routing decision.
type Router struct {
	Reasoning string `json:"reasoning"`
	Next      string `json:"next"`
}

// StepRecord stores per-step timing info for the summary table.
type StepRecord struct {
	Step    int
	Agent   string
	Elapsed time.Duration
}

// Citation is a web search source extracted from grounding metadata.
type Citation struct {
	Title string
	URL   string
}

// agentNodes is the set of specialist agent node names.
// Used for anti-loop detection: if any of these has already run once, block a repeat call.
var agentNodes = map[string]bool{
	"web_search":      true,
	"code_expert":     true,
	"data_analyst":    true,
	"security_expert": true,
	"writing_expert":  true,
	"devops_expert":   true,
}

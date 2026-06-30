# Spec: Durable Conversation Persistence — SAH

**Status**: approved, pending implementation  
**Scope**: `examples/single_agent_harness/`

---

## Core invariant

> LLM generation is killed ONLY by:
> 1. Backend process crash (unrecoverable)
> 2. User explicitly clicks **Stop**
>
> Client disconnect (F5, mobile swipe, network drop, tab switch) MUST NOT kill generation.

---

## Root causes of current fragility

### 1. Context tied to HTTP request lifecycle
```go
// current — WRONG
ctx, cancel := context.WithCancel(r.Context())
```
When client F5s → `r.Context()` cancelled → DeepSeek HTTP call aborted → generation dies silently.

**Fix**: decouple from request context.
```go
// correct
ctx, cancel := context.WithCancel(context.Background())
```
Only `cancelSession()` (from `/stop`) can cancel this context.

### 2. Frontend is source of truth for message history
```go
history := append(req.History, messageJSON{Role: "user", Content: req.Message})
```
Agent uses whatever frontend sends. After F5 or reconnect, frontend may send stale/incomplete history.

**Fix**: MongoDB `ds_messages` is authoritative. Frontend-sent history is ignored if session already exists in DB.

### 3. Tool call structure lost on persist (EI bug root cause)
Saving only `[{role, content}]` then reconstructing → loses `tool_calls` array and `tool_call_id` fields → LLM context broken on reload → agent "ngáo".

**Fix**: persist `ds_messages` (exact `[]dsChatMsg` slice) as authoritative agent context. Never reconstruct it from `ui_messages`.

---

## MongoDB schema

**Collection**: `sah_conversations`

```go
type ConversationDoc struct {
    SessionID  string      `bson:"session_id"`   // UUID, unique index
    Name       string      `bson:"name"`          // auto: first 60 chars of first user msg
    Status     string      `bson:"status"`        // "idle" | "generating"
    UIMessages []MsgUI     `bson:"ui_messages"`   // [{role, content}] — display only
    DSMessages []dsChatMsg `bson:"ds_messages"`   // full DeepSeek format — agent context source of truth
    CreatedAt  time.Time   `bson:"created_at"`
    UpdatedAt  time.Time   `bson:"updated_at"`
}

type MsgUI struct {
    Role    string `bson:"role"`    // "user" | "model"
    Content string `bson:"content"`
}
```

**Indexes**:
- `session_id` — unique
- `updated_at` descending — for listing newest-first

**Why ds_messages must be saved verbatim** — a turn with tool calls looks like:
```json
[
  {"role": "system",    "content": "..."},
  {"role": "user",      "content": "phân tích file này"},
  {"role": "assistant", "tool_calls": [{"id":"tc_1","function":{"name":"execute_python","arguments":"{...}"}}]},
  {"role": "tool",      "content": "output...", "tool_call_id": "tc_1"},
  {"role": "assistant", "content": "Kết quả là..."}
]
```
If only `ui_messages` is saved and we try to replay: `tool_calls` and `tool_call_id` are gone → DeepSeek API rejects or hallucinates.

---

## Write sequence (per turn)

```
┌─ chatHandler receives POST /chat ─────────────────────────────────┐
│                                                                    │
│  1. Load existing ds_messages from MongoDB (if session exists)     │
│     → ignore req.History entirely                                  │
│                                                                    │
│  2. Append new user msg to ui_messages + ds_messages               │
│     Set status = "generating"                                      │
│     Set name if first message (first 60 chars of user msg)         │
│     MongoDB upsert (upsert by session_id)                          │
│                                                                    │
│  3. Emit "session_status" SSE event → {status:"generating",        │
│     session_id, name} so frontend knows server accepted the turn   │
│                                                                    │
│  4. Spawn goroutine with context.Background() ←── key fix          │
│     goroutine receives its own cancel via registerCancel()         │
└───────────────────────────────────────────────────────────────────┘

┌─ goroutine: runAgentTurn ─────────────────────────────────────────┐
│                                                                    │
│  5. Run ReAct loop — uses ds_messages from DB as initial msgs[]    │
│     SSE events stream to client IF still connected                 │
│     (SSE is best-effort; MongoDB is guarantee)                     │
│                                                                    │
│  6. On each round, emit heartbeat SSE every 10s of silence         │
│     (distinguishes slow gen from dead backend)                     │
│                                                                    │
│  7. runAgentTurn returns (finalText, finalDSMsgs)                  │
│                                                                    │
│  8. MongoDB upsert:                                                │
│     - Append {role:"model", content: finalText} to ui_messages     │
│     - Replace ds_messages with finalDSMsgs (full msgs[] slice)     │
│     - Set status = "idle"                                          │
│     - Set updated_at = now()                                       │
│                                                                    │
│  9. Emit "done" SSE event (client may or may not receive it)       │
└───────────────────────────────────────────────────────────────────┘
```

### /stop flow
```
cancelSession(sessionID)
  → context cancelled in goroutine
  → StreamChatWithTools returns partial text
  → runAgentTurn returns whatever was generated
  → step 8 runs normally (partial text saved as assistant msg)
  → status = "idle"
  → ui shows partial response with no special marker (clean UX)
```

---

## Request idempotency

Each `/chat` request carries a client-generated `request_id` (UUID, generated by frontend on send).

```go
type chatRequest struct {
    Message   string `json:"message"`
    SessionID string `json:"session_id"`
    RequestID string `json:"request_id"` // new — idempotency key
    // History field kept for fallback (new sessions with no DB record)
    History   []messageJSON `json:"history"`
}
```

Backend stores `last_request_id` in the session document.  
If `request_id` matches `last_request_id` → request is a retry, skip processing, return current status.  
Prevents duplicate turns from double-send on reconnect.

---

## Startup crash recovery

On `main()` startup, before accepting requests:
```go
mongo.UpdateMany(
    filter{"status": "generating"},
    update{"$set": {"status": "idle", "interrupted": true}},
)
```

Marks interrupted sessions. Frontend can optionally show "⚠️ Generation was interrupted due to server restart" badge.

---

## Frontend reconnect flow

### Client reconnects mid-generation (no SSE stream to reattach to)
```
GET /sessions/{id}
  → { status: "generating", ui_messages: [...without current turn...] }
  → frontend shows spinner on the session
  → poll GET /sessions/{id} every 2s
  → when status = "idle": render new ui_messages (includes completed assistant turn)
```

No SSE reattachment. SSE is ephemeral. MongoDB is truth.

### Client opens app fresh (cold start)
```
GET /sessions
  → list of sessions [{session_id, name, status, updated_at, preview}]
  → sessions with status="generating" show spinner in sidebar
  → clicking one: GET /sessions/{id} → if still generating: poll; if idle: render
```

---

## SSE events (no changes to existing, additions only)

| Event | Payload | When |
|---|---|---|
| `token` | `{text}` | LLM text chunk |
| `tool_call_start` | `{name, index}` | Tool call begins |
| `tool_arg_chunk` | `{index, chunk}` | Tool arg streaming |
| `tool_result` | `{name, index, result, ...}` | Tool finished |
| `patch_diff` | `{filename, old_snippet, new_snippet}` | File patched |
| `execute_python` | `{label, output}` | Code ran |
| `usage` | `{prompt_tok, completion_tok}` | Token counts |
| `done` | `{text}` | Turn complete |
| `error` | `{message}` | Error |
| `session_status` | `{session_id, name, status}` | **NEW** — emitted at start of each turn; frontend uses this to update sidebar |
| `heartbeat` | `{}` | **NEW** — emitted every 10s during slow generation; frontend resets disconnect timer |

---

## REST API additions

| Method | Path | Description |
|---|---|---|
| `GET` | `/sessions` | List sessions (50 newest, `status`, `name`, `updated_at`, `ui_messages[0]` preview) |
| `GET` | `/sessions/{id}` | Full session (`status`, `ui_messages`, `updated_at`, `interrupted`) |
| `DELETE` | `/sessions/{id}` | Delete session + all messages |
| `PATCH` | `/sessions/{id}/name` | `{"name": "..."}` — rename |

---

## Stack additions

### docker-compose.yml
```yaml
sah-mongo:
  container_name: sah-mongo
  image: mongo:7
  volumes:
    - sah_mongo:/data/db
  ports:
    - "127.0.0.1:27017:27017"
  healthcheck:
    test: ["CMD", "mongosh", "--eval", "db.adminCommand('ping')"]
    interval: 5s
    timeout: 5s
    retries: 10
  restart: unless-stopped
```

`sah-go-agent-core` adds `depends_on: sah-mongo: healthy`.

### .env
```
MONGO_URI=mongodb://localhost:27017      # host-mode dev
MONGO_DB=sah                            # database name
# Docker mode overrides MONGO_URI to mongodb://sah-mongo:27017
```

### go.mod
```
go.mongodb.org/mongo-driver/v2
```

### New file: mongo.go
Functions:
- `initMongo()` — connect, ensure indexes, crash recovery (reset "generating")
- `shutdownMongo()`
- `upsertUserTurn(sessionID, name, userMsg)` — step 2
- `upsertAssistantTurn(sessionID, finalText, finalDSMsgs []dsChatMsg)` — step 8
- `markGenerating(sessionID, reqID)` — idempotency check + status set
- `loadDSMessages(sessionID) []dsChatMsg` — load agent context from DB
- `listSessions(limit int) []SessionPreview`
- `getSession(sessionID) *ConversationDoc`
- `deleteSession(sessionID)`
- `renameSession(sessionID, name)`

---

## Generation stop reason tracking

Every assistant turn saved to MongoDB must carry a `stop_reason` field so the frontend and future billing/audit systems can distinguish why generation ended.

### Schema addition

```go
type ConversationDoc struct {
    // ... existing fields ...
    LastStopReason string `bson:"last_stop_reason"` // see values below
}
```

Field is set alongside `status = "idle"` at the end of every turn (step 8).

### Stop reason values

| Value | Trigger | Set by | Frontend hint |
|---|---|---|---|
| `"completed"` | ReAct loop reached final answer normally | goroutine after `runAgentTurn` returns with full text | *(nothing — normal UX)* |
| `"user_stopped"` | User clicked Stop → `cancelSession()` called | `/stop` handler before cancel; goroutine confirms on return | `[dừng]` badge on last assistant bubble |
| `"llm_error"` | DeepSeek API returned error (4xx/5xx, network failure) | goroutine when `StreamChatWithTools` returns non-nil err AND `ctx.Err() == nil` | `[lỗi API]` badge + error message visible |
| `"max_rounds"` | ReAct loop hit `maxReActRounds` without final answer | goroutine after loop exits with no text | `[vòng lặp tối đa]` badge |
| `"interrupted"` | Backend process crashed mid-generation | startup crash recovery (`UpdateMany` on stuck "generating") | `[gián đoạn — server restart]` badge |
| `"context_error"` | ctx cancelled for any reason other than user_stopped (edge case) | goroutine when `ctx.Err() != nil` AND `/stop` was NOT the cause | `[gián đoạn]` badge |

### How to distinguish user_stopped vs context_error

`cancelSession()` sets a flag before cancelling:

```go
// in-memory map — lives only as long as process
var stopReasonsMu sync.Mutex
var stopReasons = map[string]string{} // sessionID → reason

func cancelSession(sessionID string) bool {
    stopReasonsMu.Lock()
    stopReasons[sessionID] = "user_stopped"
    stopReasonsMu.Unlock()
    // ... existing cancel logic ...
}

func consumeStopReason(sessionID string) string {
    stopReasonsMu.Lock()
    defer stopReasonsMu.Unlock()
    r := stopReasons[sessionID]
    delete(stopReasons, sessionID)
    if r == "" {
        return "context_error"
    }
    return r
}
```

In the goroutine after `runAgentTurn` returns:

```go
answer := runAgentTurn(ctx, sessionID, traceID, history, eventCh)

stopReason := "completed"
switch {
case ctx.Err() != nil:
    stopReason = consumeStopReason(sessionID) // "user_stopped" or "context_error"
case answer == "" && noToolsRan:
    stopReason = "llm_error"
case hitMaxRounds:
    stopReason = "max_rounds"
}

mongo.UpsertAssistantTurn(sessionID, answer, finalDSMsgs, stopReason)
```

`runAgentTurn` returns two extra values to support this:

```go
func runAgentTurn(...) (finalText string, hitMaxRounds bool, finalDSMsgs []dsChatMsg)
```

### Startup crash recovery (updated)

```go
mongo.UpdateMany(
    filter{"status": "generating"},
    update{"$set": {
        "status":          "idle",
        "last_stop_reason": "interrupted",
    }},
)
```

### SSE event carrying stop reason

The `done` event already exists. Extend payload:

```json
{ "type": "done", "text": "...", "stop_reason": "completed" }
```

Frontend uses this to immediately show the correct badge without waiting for a poll.

---

## What NOT to do

- Do NOT save to MongoDB inside the SSE drain loop (that's tied to client connection)
- Do NOT use ui_messages to reconstruct ds_messages (the EI bug)
- Do NOT cancel agent context on client disconnect
- Do NOT store binary/base64 content in messages (strip before save, same as EI)
- Do NOT implement SSE stream reattachment (complexity not worth it; poll is sufficient)

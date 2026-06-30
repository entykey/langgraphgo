# Single-Agent Harness (SAH)

Go ReAct agent lab — DeepSeek LLM + Gemini VLM/Search, sandboxed Python execution, skill system, artifact management, and a full observability stack.

## Stack

| Container | Role | Port |
|---|---|---|
| `sah-go-agent-core` | Go ReAct agent (HTTP + SSE) | `8080` |
| `sah-langfuse-web` | Trace UI + API | `3100` |
| `sah-langfuse-worker` | Async trace processor | — |
| `sah-clickhouse` | ClickHouse (Langfuse + agent metrics) | `127.0.0.1:8124` |
| `sah-kafka` | Kafka KRaft (agent turn events) | `127.0.0.1:9094` (host) |
| `sah-postgres` | PostgreSQL (Langfuse metadata) | `127.0.0.1:5533` |
| `sah-redis` | Redis (Langfuse queue) | — |
| `sah-minio` | MinIO S3 (artifacts + Langfuse blobs) | `127.0.0.1:9000` |
| `sah-minio-init` | One-shot bucket creation | — |
| `sah-clickhouse-init` | One-shot ClickHouse schema init | — |

## Run

```bash
# Start full stack (builds Go agent image)
docker compose up -d --build

# Dev mode — run agent on host, infra in Docker
docker compose up -d   # skip --build, sah-go-agent-core optional
cd examples && go run ./single_agent_harness/
```

Open **http://localhost:8080** for the chat UI, **http://localhost:3100** for Langfuse traces.

## Event pipeline

Every LLM turn is pushed asynchronously to Kafka → ClickHouse:

```
Go agent  →  Kafka (sah.agent.turns)  →  ClickHouse Kafka engine  →  MergeTree
```

- **Kafka**: `sah-kafka:9092` (internal) / `localhost:9094` (host dev)
- **Topic**: `sah.agent.turns` — auto-created on first agent start
- **Schema**: `clickhouse/schema.sql` — applied automatically by `sah-clickhouse-init`

## ClickHouse queries

### Recent turns

```sql
SELECT formatDateTime(ts,'%H:%i:%S') AS time,
       session_id, round, gateway,
       ttft_ms, gen_ms, round(tok_per_sec) AS tps,
       response_type, substr(user_msg,1,60) AS msg
FROM sah_agent_turns
ORDER BY ts DESC LIMIT 20
FORMAT PrettyCompact
```

```
┌─time─────┬─session_id───────────────────────────┬─round─┬─gateway──────────┬─ttft_ms─┬─gen_ms─┬─tps─┬─response_type─┬─msg────────────────────────────────────────┐
1. │ 03:01:12 │ 439f6579-ee3e-4af5-8466-3609ef8acdd0 │     1 │ api.deepseek.com │     554 │   2585 │  57 │ text          │ lỡ cả đời ko rực rỡ thì sao ? trả lời ngắn │
2. │ 03:01:07 │ 439f6579-ee3e-4af5-8466-3609ef8acdd0 │     1 │ api.deepseek.com │    1006 │   2988 │  67 │ text          │ Xin chào, tôi là Tuấn                      │
   └──────────┴──────────────────────────────────────┴───────┴──────────────────┴─────────┴────────┴─────┴───────────────┴────────────────────────────────────────────┘
   ┌─time─────┬─session_id───────────────────────────┬─round─┬─gateway──────────┬─ttft_ms─┬─gen_ms─┬─tps─┬─response_type─┬─msg────────────────────────────────────────┐
3. │ 02:49:45 │ 3c8a7a9a-6463-4cfd-8933-40811904b0ef │     1 │ api.deepseek.com │     716 │   1931 │  51 │ text          │ lỡ cả đời ko rực rỡ thì sao ? trả lời ngắn │
   └──────────┴──────────────────────────────────────┴───────┴──────────────────┴─────────┴────────┴─────┴───────────────┴────────────────────────────────────────────┘
   ┌─time─────┬─session_id───────────────────────────┬─round─┬─gateway──────────┬─ttft_ms─┬─gen_ms─┬─tps─┬─response_type─┬─msg────────────────────────────────────────┐
4. │ 02:48:36 │ eba30002-2ce0-4e83-b33f-7c9bc8ef28d5 │     1 │ api.deepseek.com │     806 │   1301 │  29 │ text          │ lỡ cả đời ko rực rỡ thì sao ? trả lời ngắn │
5. │ 02:46:45 │ eba30002-2ce0-4e83-b33f-7c9bc8ef28d5 │     1 │ api.deepseek.com │    1104 │   2344 │  44 │ text          │ lỡ cả đời ko rực rỡ thì sao ? trả lời ngắn │
   └──────────┴──────────────────────────────────────┴───────┴──────────────────┴─────────┴────────┴─────┴───────────────┴────────────────────────────────────────────┘
```

### Per-session summary (today)

```sql
SELECT session_id, count() AS turns,
       sum(prompt_tok+complete_tok) AS total_tok,
       round(avg(ttft_ms)) AS avg_ttft_ms,
       round(avg(gen_ms))  AS avg_gen_ms
FROM sah_agent_turns
WHERE ts >= today()
GROUP BY session_id
ORDER BY turns DESC
FORMAT PrettyCompact
```

```
   ┌─session_id───────────────────────────┬─turns─┬─total_tok─┬─avg_ttft_ms─┬─avg_gen_ms─┐
1. │ eba30002-2ce0-4e83-b33f-7c9bc8ef28d5 │     2 │      7078 │         955 │       1822 │
2. │ 439f6579-ee3e-4af5-8466-3609ef8acdd0 │     2 │      7351 │         780 │       2786 │
3. │ 3c8a7a9a-6463-4cfd-8933-40811904b0ef │     1 │      3499 │         716 │       1931 │
   └──────────┴──────────────────────────────────────┴───────┴───────────┴─────────────┴────────────┘
```

### Token spend per hour (last 7 days)

```sql
SELECT formatDateTime(toStartOfHour(ts),'%m-%d %H:00') AS hour,
       model,
       sum(prompt_tok)  AS p_tok,
       sum(complete_tok) AS c_tok,
       round(avg(tok_per_sec)) AS avg_tps
FROM sah_agent_turns
WHERE ts >= now() - INTERVAL 7 DAY
GROUP BY hour, model
ORDER BY hour
FORMAT PrettyCompact
```

```
   ┌─hour────────┬─model─────────────┬─p_tok─┬─c_tok─┬─avg_tps─┐
1. │ 06-30 02:00 │ deepseek-v4-flash │ 10338 │   239 │      41 │
2. │ 06-30 03:00 │ deepseek-v4-flash │  7002 │   349 │      62 │
   └─────────────┴───────────────────┴───────┴───────┴─────────┘
```

### All-time totals

```sql
SELECT count()          AS turns,
       sum(prompt_tok)  AS prompt,
       sum(complete_tok) AS complete,
       round(avg(ttft_ms))    AS avg_ttft,
       round(avg(tok_per_sec)) AS avg_tps
FROM sah_agent_turns
FORMAT PrettyCompact
```

```
   ┌─turns─┬─prompt─┬─complete─┬─avg_ttft─┬─avg_tps─┐
1. │     5 │  17340 │      588 │      837 │      50 │
   └───────┴────────┴──────────┴──────────┴─────────┘
```

### Run queries

```powershell
# Shorthand — replace <QUERY> with any block above
docker exec sah-clickhouse clickhouse-client `
  --user clickhouse --password sah_click_pw `
  --query "<SQL_QUERY>"
```

Example:
```powershell
docker exec sah-clickhouse clickhouse-client `
  --user clickhouse --password sah_click_pw `
  --query "SELECT formatDateTime(ts,'%H:%i:%S') AS time,
       session_id, round, gateway,
       ttft_ms, gen_ms, round(tok_per_sec) AS tps,
       response_type, substr(user_msg,1,60) AS msg
FROM sah_agent_turns
ORDER BY ts DESC LIMIT 20
FORMAT PrettyCompact"
```


## Env vars (`.env`)

| Key | Default | Notes |
|---|---|---|
| `DEEPSEEK_API_KEY` | — | Required |
| `GOOGLE_API_KEY` | — | Required (Gemini search + VLM) |
| `AGENT_MODEL` | `deepseek-v4-flash` | DeepSeek model |
| `SEARCH_MODEL` | `gemini-3.1-flash-lite` | Gemini search model |
| `KAFKA_BROKERS` | `localhost:9094` | Host dev; Docker overrides to `sah-kafka:9092` |
| `KAFKA_TOPIC` | `sah.agent.turns` | |
| `LOG_AGENT` | `true` | Structured per-turn log to stdout (VN time UTC+7) |
| `LANGFUSE_BASE_URL` | `http://localhost:3100` | |

-- ClickHouse schema for SAH agent turn events
-- Pipeline: Go agent → Kafka (sah.agent.turns) → Kafka engine table → MV → MergeTree
--
-- Apply once against your ClickHouse instance:
--   clickhouse-client --query "$(cat schema.sql)"

-- 1. Kafka engine table (raw ingest — do not query directly)
CREATE TABLE IF NOT EXISTS sah_agent_turns_kafka
(
    event_type    String,
    ts            String,   -- RFC3339 UTC, e.g. "2026-06-30T08:33:47Z"
    session_id    String,
    round         UInt32,
    model         String,
    gateway       String,
    connect_ms    Float64,
    ttft_ms       Float64,
    gen_ms        Float64,
    prompt_tok    UInt32,
    complete_tok  UInt32,
    tok_per_sec   Float64,
    response_type String,   -- "text" | "tools"
    user_msg      String
)
ENGINE = Kafka
SETTINGS
    kafka_broker_list      = 'sah-kafka:9092',    -- internal Docker network name
    kafka_topic_list       = 'sah.agent.turns',
    kafka_group_name       = 'clickhouse-sah-turns',
    kafka_format           = 'JSONEachRow',
    kafka_num_consumers    = 1,
    kafka_skip_broken_messages = 10;

-- 2. MergeTree storage table (query this one)
CREATE TABLE IF NOT EXISTS sah_agent_turns
(
    event_type    LowCardinality(String),
    ts            DateTime64(3, 'UTC'),
    session_id    String,
    round         UInt32,
    model         LowCardinality(String),
    gateway       LowCardinality(String),
    connect_ms    Float64,
    ttft_ms       Float64,
    gen_ms        Float64,
    prompt_tok    UInt32,
    complete_tok  UInt32,
    tok_per_sec   Float64,
    response_type LowCardinality(String),
    user_msg      String
)
ENGINE = MergeTree
PARTITION BY toYYYYMM(ts)
ORDER BY (session_id, ts)
TTL toDateTime(ts) + INTERVAL 90 DAY;

-- 3. Materialized view: Kafka → MergeTree (parses ts string → DateTime64)
CREATE MATERIALIZED VIEW IF NOT EXISTS sah_agent_turns_mv
TO sah_agent_turns
AS
SELECT
    event_type,
    parseDateTime64BestEffort(ts, 3, 'UTC') AS ts,
    session_id,
    round,
    model,
    gateway,
    connect_ms,
    ttft_ms,
    gen_ms,
    prompt_tok,
    complete_tok,
    tok_per_sec,
    response_type,
    user_msg
FROM sah_agent_turns_kafka;

-- ── Useful queries ────────────────────────────────────────────────────────────

-- Per-session summary (today)
-- SELECT session_id, count() AS turns, sum(prompt_tok + complete_tok) AS total_tok,
--        avg(ttft_ms) AS avg_ttft, avg(gen_ms) AS avg_gen
-- FROM sah_agent_turns
-- WHERE ts >= today()
-- GROUP BY session_id
-- ORDER BY turns DESC;

-- Hourly token spend (last 7 days)
-- SELECT toStartOfHour(ts) AS hour, model,
--        sum(prompt_tok) AS p_tok, sum(complete_tok) AS c_tok
-- FROM sah_agent_turns
-- WHERE ts >= now() - INTERVAL 7 DAY
-- GROUP BY hour, model
-- ORDER BY hour;

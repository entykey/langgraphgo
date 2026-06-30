#!/bin/sh
# SAH ClickHouse schema init — runs via sah-clickhouse-init on every deploy.
# Uses HTTP API to avoid depending on clickhouse-client binary in init image.
set -e

URL="http://sah-clickhouse:8123/?user=clickhouse&password=${CH_PW}"

sql() {
  curl -sf --data-binary "$1" "$URL"
}

echo "[clickhouse-init] applying schema to sah-clickhouse..."

sql "CREATE TABLE IF NOT EXISTS sah_agent_turns_kafka
(
    event_type    String,
    ts            String,
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
    response_type String,
    user_msg      String
)
ENGINE = Kafka
SETTINGS
    kafka_broker_list         = 'sah-kafka:9092',
    kafka_topic_list          = 'sah.agent.turns',
    kafka_group_name          = 'clickhouse-sah-turns',
    kafka_format              = 'JSONEachRow',
    kafka_num_consumers       = 1,
    kafka_skip_broken_messages = 10"

sql "CREATE TABLE IF NOT EXISTS sah_agent_turns
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
TTL toDateTime(ts) + INTERVAL 90 DAY"

sql "CREATE MATERIALIZED VIEW IF NOT EXISTS sah_agent_turns_mv
TO sah_agent_turns
AS SELECT
    event_type,
    parseDateTime64BestEffort(ts, 3, 'UTC') AS ts,
    session_id, round, model, gateway,
    connect_ms, ttft_ms, gen_ms,
    prompt_tok, complete_tok, tok_per_sec,
    response_type, user_msg
FROM sah_agent_turns_kafka"

echo "[clickhouse-init] done"

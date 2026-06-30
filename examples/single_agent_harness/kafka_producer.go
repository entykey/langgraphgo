package main

import (
	"context"
	"encoding/json"
	"fmt"
	"net"
	"os"
	"strings"
	"time"

	kafka "github.com/segmentio/kafka-go"
)

// TurnEvent is the schema for one LLM round pushed to Kafka → ClickHouse.
type TurnEvent struct {
	EventType    string  `json:"event_type"` // "agent.turn"
	Ts           string  `json:"ts"`         // RFC3339 UTC timestamp
	SessionID    string  `json:"session_id"`
	Round        int     `json:"round"`
	Model        string  `json:"model"`
	Gateway      string  `json:"gateway"` // host only, e.g. "api.deepseek.com"
	ConnectMS    float64 `json:"connect_ms"`
	TTFT_MS      float64 `json:"ttft_ms"`
	GenMS        float64 `json:"gen_ms"`
	PromptTok    int     `json:"prompt_tok"`
	CompleteTok  int     `json:"complete_tok"`
	TokPerSec    float64 `json:"tok_per_sec"`
	ResponseType string  `json:"response_type"` // "text" or "tools"
	UserMsg      string  `json:"user_msg"`      // first 200 chars of user message
}

var (
	kWriter    *kafka.Writer
	kafkaOn    bool
	kafkaTopic string
)

// initKafka reads KAFKA_BROKERS / KAFKA_TOPIC from env and sets up an async writer.
// If KAFKA_BROKERS is unset or empty, Kafka logging is disabled silently.
func initKafka() {
	brokers := strings.TrimSpace(os.Getenv("KAFKA_BROKERS"))
	if brokers == "" {
		return
	}
	kafkaTopic = getEnv("KAFKA_TOPIC", "sah.agent.turns")
	addrs := strings.Split(brokers, ",")
	ensureKafkaTopic(addrs, kafkaTopic)

	kWriter = &kafka.Writer{
		Addr:         kafka.TCP(addrs...),
		Topic:        kafkaTopic,
		Async:        true,
		BatchSize:    100,
		BatchTimeout: 50 * time.Millisecond,
		MaxAttempts:  3,
		Balancer:     &kafka.LeastBytes{},
		ErrorLogger: kafka.LoggerFunc(func(msg string, args ...interface{}) {
			fmt.Fprintf(os.Stderr, "[kafka] "+msg+"\n", args...)
		}),
	}
	kafkaOn = true
	fmt.Printf("  kafka  = %s  topic=%s\n", brokers, kafkaTopic)
}

// ensureKafkaTopic creates the topic if it doesn't exist, so the first produce
// never hits LEADER_NOT_AVAILABLE (which MaxAttempts:1 would have dropped).
func ensureKafkaTopic(brokerAddrs []string, topic string) {
	conn, err := kafka.Dial("tcp", brokerAddrs[0])
	if err != nil {
		fmt.Fprintf(os.Stderr, "[kafka] dial for topic init: %v\n", err)
		return
	}
	defer conn.Close()

	// Find the controller to create topics.
	controller, err := conn.Controller()
	if err != nil {
		fmt.Fprintf(os.Stderr, "[kafka] controller lookup: %v\n", err)
		return
	}
	ctrlConn, err := kafka.Dial("tcp", net.JoinHostPort(controller.Host, fmt.Sprint(controller.Port)))
	if err != nil {
		fmt.Fprintf(os.Stderr, "[kafka] controller dial: %v\n", err)
		return
	}
	defer ctrlConn.Close()

	err = ctrlConn.CreateTopics(kafka.TopicConfig{
		Topic:             topic,
		NumPartitions:     1,
		ReplicationFactor: 1,
	})
	if err != nil {
		// TOPIC_ALREADY_EXISTS is not an error — log only unexpected errors.
		if !strings.Contains(err.Error(), "already exists") {
			fmt.Fprintf(os.Stderr, "[kafka] create topic %q: %v\n", topic, err)
		}
	}
}

// shutdownKafka flushes and closes the async writer (call on process exit).
func shutdownKafka() {
	if kWriter == nil {
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	// Writer.Close() drains the batch queue — blocks until flushed or timeout.
	done := make(chan error, 1)
	go func() { done <- kWriter.Close() }()
	select {
	case err := <-done:
		if err != nil {
			fmt.Fprintf(os.Stderr, "[kafka] close error: %v\n", err)
		}
	case <-ctx.Done():
		fmt.Fprintln(os.Stderr, "[kafka] flush timeout — some events may be lost")
	}
}

// emitEvent serialises v to JSON and fires it toward Kafka. Always non-blocking:
// the actual WriteMessages call runs in a detached goroutine so the agent loop
// is never delayed by broker latency or unreachable brokers.
// Falls back to a stderr line when Kafka is not configured.
func emitEvent(v any) {
	data, err := json.Marshal(v)
	if err != nil {
		return
	}
	if !kafkaOn {
		fmt.Fprintf(os.Stderr, "[event] %s\n", data)
		return
	}
	go func() {
		_ = kWriter.WriteMessages(context.Background(), kafka.Message{Value: data})
	}()
}

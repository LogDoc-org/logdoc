package notify

import (
	"encoding/json"
	"testing"
	"time"
)

func TestKafkaBody(t *testing.T) {
	ts := time.Date(2026, 8, 19, 12, 0, 0, 0, time.UTC)
	ev := Event{
		Rule: "billing cascade", Type: TypeErrorThreshold, App: "billing",
		Count: 2, Window: "1m0s", Ts: ts, Message: "billing cascade: 2 ...",
		Entries: []EventEntry{
			{Ts: ts, App: "billing", Lvl: "ERROR", Msg: "pool exhausted",
				Fields: map[string]string{"trace_id": "abc"}},
			{Ts: ts, App: "billing", Lvl: "SEVERE", Msg: "charge failed"},
		},
	}

	var got map[string]any
	if err := json.Unmarshal(kafkaBody(ev), &got); err != nil {
		t.Fatal(err)
	}
	if got["rule"] != "billing cascade" || got["server_time"] != "2026-08-19T12:00:00Z" {
		t.Fatalf("envelope: %v", got)
	}
	entries, ok := got["entries"].([]any)
	if !ok || len(entries) != 2 {
		t.Fatalf("entries: %v", got["entries"])
	}
	first := entries[0].(map[string]any)
	if first["msg"] != "pool exhausted" || first["lvl"] != "ERROR" {
		t.Fatalf("first entry: %v", first)
	}
	if first["fields"].(map[string]any)["trace_id"] != "abc" {
		t.Fatalf("fields lost: %v", first)
	}

	// No matched entries (silence): entries must be [], not null.
	var silent map[string]any
	_ = json.Unmarshal(kafkaBody(Event{Rule: "s", Type: TypeSilence, Ts: ts}), &silent)
	if v, ok := silent["entries"].([]any); !ok || len(v) != 0 {
		t.Fatalf("silence entries: %v", silent["entries"])
	}
}

func TestKafkaSenderConfigured(t *testing.T) {
	senders := buildSenders(Config{Kafka: KafkaConfig{Brokers: []string{"localhost:9092"}, Topic: "alerts"}})
	if len(senders) != 1 || senders[0].Name() != "kafka" {
		t.Fatalf("senders: %v", senders)
	}
	// Incomplete config (no topic) must not build a sender.
	if s := buildSenders(Config{Kafka: KafkaConfig{Brokers: []string{"localhost:9092"}}}); len(s) != 0 {
		t.Fatalf("half-configured kafka built: %v", s)
	}
}

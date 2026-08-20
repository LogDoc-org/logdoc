package notify

import (
	"context"
	"encoding/json"
	"time"

	kafka "github.com/segmentio/kafka-go"
)

// KafkaConfig — the Kafka notification channel (P2): fired events land in a
// topic as JSON, one message per fire, keyed by the rule name.
type KafkaConfig struct {
	Brokers []string `yaml:"brokers"`
	Topic   string   `yaml:"topic"`
}

// kafkaPayload — the message envelope: the rule that fired, every matched
// entry behind it, and the server time.
type kafkaPayload struct {
	Rule       string       `json:"rule"`
	Type       string       `json:"type"`
	App        string       `json:"app,omitempty"`
	Count      int          `json:"count,omitempty"`
	Window     string       `json:"window"`
	Message    string       `json:"message"`
	Entries    []EventEntry `json:"entries"`
	ServerTime time.Time    `json:"server_time"`
}

func kafkaBody(ev Event) []byte {
	entries := ev.Entries
	if entries == nil {
		entries = []EventEntry{}
	}
	body, _ := json.Marshal(kafkaPayload{
		Rule: ev.Rule, Type: ev.Type, App: ev.App, Count: ev.Count,
		Window: ev.Window, Message: ev.Message,
		Entries: entries, ServerTime: ev.Ts,
	})
	return body
}

// Kafka delivers events to a topic — a ported v1 Kafker pipe plugin, minus
// its bug of dropping the matched entries from the payload.
type Kafka struct {
	w *kafka.Writer
}

func newKafka(cfg KafkaConfig) *Kafka {
	return &Kafka{w: &kafka.Writer{
		Addr:                   kafka.TCP(cfg.Brokers...),
		Topic:                  cfg.Topic,
		Balancer:               &kafka.Hash{},
		AllowAutoTopicCreation: true,
	}}
}

func (k *Kafka) Name() string { return "kafka" }

func (k *Kafka) Send(ctx context.Context, ev Event) error {
	return k.w.WriteMessages(ctx, kafka.Message{Key: []byte(ev.Rule), Value: kafkaBody(ev)})
}

func (k *Kafka) Close() error { return k.w.Close() }

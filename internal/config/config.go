// Package config loads the LogDoc configuration.
// Priority (lowest to highest): defaults → yaml file → env (LOGDOC_*) → flags.
package config

import (
	"flag"
	"fmt"
	"os"
	"time"

	"gopkg.in/yaml.v3"

	"github.com/LogDoc-org/logdoc/internal/notify"
)

type Config struct {
	HTTP       HTTP          `yaml:"http"`
	Ingest     Ingest        `yaml:"ingest"`
	ClickHouse ClickHouse    `yaml:"clickhouse"`
	Graph      Graph         `yaml:"graph"`
	Notify     notify.Config `yaml:"notify"`
	Log        Log           `yaml:"log"`
}

type Graph struct {
	// DBPath — the embedded SQLite file with the Architecture Graph state.
	DBPath string `yaml:"db_path"`
}

type HTTP struct {
	// Addr — address for API + UI (single port, same as v1: 9001).
	Addr string `yaml:"addr"`
}

type Ingest struct {
	// APIKey — the single key of the S1 single-user mode.
	// An empty key = ingest without authorization (dev mode).
	APIKey string `yaml:"api_key"`
	Native Native `yaml:"native"`
	OTLP   OTLP   `yaml:"otlp"`
	Syslog Syslog `yaml:"syslog"`
}

type Syslog struct {
	// TCPAddr/UDPAddr — syslog listeners (RFC 3164 + RFC 5424, auto-detected).
	// Empty (default) disables the listener.
	TCPAddr string `yaml:"tcp_addr"`
	UDPAddr string `yaml:"udp_addr"`
}

type OTLP struct {
	// GRPCAddr — OTLP/gRPC logs listener address (standard port 4317).
	// Empty string disables the listener.
	GRPCAddr string `yaml:"grpc_addr"`
}

type Native struct {
	// TCPAddr/UDPAddr — listeners for the v1-compatible ld_format protocol.
	TCPAddr string `yaml:"tcp_addr"`
	UDPAddr string `yaml:"udp_addr"`
}

type ClickHouse struct {
	Addr     string `yaml:"addr"`
	Database string `yaml:"database"`
	Username string `yaml:"username"`
	Password string `yaml:"password"`

	// Batching-writer parameters.
	BatchSize     int           `yaml:"batch_size"`
	FlushInterval time.Duration `yaml:"flush_interval"`

	// TTLDays — log retention period in days.
	TTLDays int `yaml:"ttl_days"`
}

type Log struct {
	Level string `yaml:"level"` // debug|info|warn|error
}

func defaults() Config {
	return Config{
		HTTP: HTTP{Addr: ":9001"},
		Ingest: Ingest{
			Native: Native{TCPAddr: ":9999", UDPAddr: ":9999"},
			OTLP:   OTLP{GRPCAddr: ":4317"},
		},
		ClickHouse: ClickHouse{
			Addr:          "localhost:9010",
			Database:      "logdoc",
			Username:      "default",
			BatchSize:     10000,
			FlushInterval: time.Second,
			TTLDays:       30,
		},
		Graph:  Graph{DBPath: "logdoc-graph.db"},
		Notify: notify.Config{RulesPath: "logdoc-rules.json"},
		Log:    Log{Level: "info"},
	}
}

// Load assembles the configuration from a file (if given), env and command-line arguments.
func Load(args []string) (Config, error) {
	cfg := defaults()

	fs := flag.NewFlagSet("logdoc", flag.ContinueOnError)
	cfgPath := fs.String("config", os.Getenv("LOGDOC_CONFIG"), "path to yaml config")
	httpAddr := fs.String("http-addr", "", "HTTP API/UI address (e.g. :9001)")
	tcpAddr := fs.String("native-tcp-addr", "", "ld_format TCP listener address")
	udpAddr := fs.String("native-udp-addr", "", "ld_format UDP listener address")
	otlpAddr := fs.String("otlp-grpc-addr", "", "OTLP/gRPC logs listener address (e.g. :4317)")
	syslogTCP := fs.String("syslog-tcp-addr", "", "syslog TCP listener address (e.g. :5140)")
	syslogUDP := fs.String("syslog-udp-addr", "", "syslog UDP listener address (e.g. :5140)")
	apiKey := fs.String("api-key", "", "ingest/query API key")
	chAddr := fs.String("clickhouse-addr", "", "ClickHouse address (host:port, native)")
	chDB := fs.String("clickhouse-db", "", "ClickHouse database")
	chUser := fs.String("clickhouse-user", "", "ClickHouse user")
	chPass := fs.String("clickhouse-password", "", "ClickHouse password")
	graphDB := fs.String("graph-db-path", "", "SQLite file for the architecture graph")
	logLevel := fs.String("log-level", "", "log level: debug|info|warn|error")

	if err := fs.Parse(args); err != nil {
		return Config{}, err
	}

	if *cfgPath != "" {
		data, err := os.ReadFile(*cfgPath)
		if err != nil {
			return Config{}, fmt.Errorf("reading config %s: %w", *cfgPath, err)
		}
		if err := yaml.Unmarshal(data, &cfg); err != nil {
			return Config{}, fmt.Errorf("parsing config %s: %w", *cfgPath, err)
		}
	}

	applyEnv(&cfg)

	// Flags take the highest priority.
	setIf(&cfg.HTTP.Addr, *httpAddr)
	setIf(&cfg.Ingest.Native.TCPAddr, *tcpAddr)
	setIf(&cfg.Ingest.Native.UDPAddr, *udpAddr)
	setIf(&cfg.Ingest.OTLP.GRPCAddr, *otlpAddr)
	setIf(&cfg.Ingest.Syslog.TCPAddr, *syslogTCP)
	setIf(&cfg.Ingest.Syslog.UDPAddr, *syslogUDP)
	setIf(&cfg.Ingest.APIKey, *apiKey)
	setIf(&cfg.ClickHouse.Addr, *chAddr)
	setIf(&cfg.ClickHouse.Database, *chDB)
	setIf(&cfg.ClickHouse.Username, *chUser)
	setIf(&cfg.ClickHouse.Password, *chPass)
	setIf(&cfg.Graph.DBPath, *graphDB)
	setIf(&cfg.Log.Level, *logLevel)

	return cfg, nil
}

func applyEnv(cfg *Config) {
	setIf(&cfg.HTTP.Addr, os.Getenv("LOGDOC_HTTP_ADDR"))
	setIf(&cfg.Ingest.Native.TCPAddr, os.Getenv("LOGDOC_NATIVE_TCP_ADDR"))
	setIf(&cfg.Ingest.Native.UDPAddr, os.Getenv("LOGDOC_NATIVE_UDP_ADDR"))
	setIf(&cfg.Ingest.OTLP.GRPCAddr, os.Getenv("LOGDOC_OTLP_GRPC_ADDR"))
	setIf(&cfg.Ingest.Syslog.TCPAddr, os.Getenv("LOGDOC_SYSLOG_TCP_ADDR"))
	setIf(&cfg.Ingest.Syslog.UDPAddr, os.Getenv("LOGDOC_SYSLOG_UDP_ADDR"))
	setIf(&cfg.Ingest.APIKey, os.Getenv("LOGDOC_API_KEY"))
	setIf(&cfg.ClickHouse.Addr, os.Getenv("LOGDOC_CLICKHOUSE_ADDR"))
	setIf(&cfg.ClickHouse.Database, os.Getenv("LOGDOC_CLICKHOUSE_DB"))
	setIf(&cfg.ClickHouse.Username, os.Getenv("LOGDOC_CLICKHOUSE_USER"))
	setIf(&cfg.ClickHouse.Password, os.Getenv("LOGDOC_CLICKHOUSE_PASSWORD"))
	setIf(&cfg.Graph.DBPath, os.Getenv("LOGDOC_GRAPH_DB_PATH"))
	setIf(&cfg.Log.Level, os.Getenv("LOGDOC_LOG_LEVEL"))
	// Notification channel secrets can live outside the yaml file.
	setIf(&cfg.Notify.Telegram.Token, os.Getenv("LOGDOC_TELEGRAM_TOKEN"))
	setIf(&cfg.Notify.Webhook.URL, os.Getenv("LOGDOC_WEBHOOK_URL"))
	setIf(&cfg.Notify.Email.Password, os.Getenv("LOGDOC_SMTP_PASSWORD"))
}

func setIf(dst *string, v string) {
	if v != "" {
		*dst = v
	}
}

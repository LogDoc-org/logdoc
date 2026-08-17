// Package config loads the LogDoc configuration.
// Priority (lowest to highest): defaults → yaml file → env (LOGDOC_*) → flags.
package config

import (
	"flag"
	"fmt"
	"os"
	"time"

	"gopkg.in/yaml.v3"
)

type Config struct {
	HTTP       HTTP       `yaml:"http"`
	Ingest     Ingest     `yaml:"ingest"`
	ClickHouse ClickHouse `yaml:"clickhouse"`
	Log        Log        `yaml:"log"`
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
		},
		ClickHouse: ClickHouse{
			Addr:          "localhost:9010",
			Database:      "logdoc",
			Username:      "default",
			BatchSize:     10000,
			FlushInterval: time.Second,
			TTLDays:       30,
		},
		Log: Log{Level: "info"},
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
	apiKey := fs.String("api-key", "", "ingest/query API key")
	chAddr := fs.String("clickhouse-addr", "", "ClickHouse address (host:port, native)")
	chDB := fs.String("clickhouse-db", "", "ClickHouse database")
	chUser := fs.String("clickhouse-user", "", "ClickHouse user")
	chPass := fs.String("clickhouse-password", "", "ClickHouse password")
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
	setIf(&cfg.Ingest.APIKey, *apiKey)
	setIf(&cfg.ClickHouse.Addr, *chAddr)
	setIf(&cfg.ClickHouse.Database, *chDB)
	setIf(&cfg.ClickHouse.Username, *chUser)
	setIf(&cfg.ClickHouse.Password, *chPass)
	setIf(&cfg.Log.Level, *logLevel)

	return cfg, nil
}

func applyEnv(cfg *Config) {
	setIf(&cfg.HTTP.Addr, os.Getenv("LOGDOC_HTTP_ADDR"))
	setIf(&cfg.Ingest.Native.TCPAddr, os.Getenv("LOGDOC_NATIVE_TCP_ADDR"))
	setIf(&cfg.Ingest.Native.UDPAddr, os.Getenv("LOGDOC_NATIVE_UDP_ADDR"))
	setIf(&cfg.Ingest.APIKey, os.Getenv("LOGDOC_API_KEY"))
	setIf(&cfg.ClickHouse.Addr, os.Getenv("LOGDOC_CLICKHOUSE_ADDR"))
	setIf(&cfg.ClickHouse.Database, os.Getenv("LOGDOC_CLICKHOUSE_DB"))
	setIf(&cfg.ClickHouse.Username, os.Getenv("LOGDOC_CLICKHOUSE_USER"))
	setIf(&cfg.ClickHouse.Password, os.Getenv("LOGDOC_CLICKHOUSE_PASSWORD"))
	setIf(&cfg.Log.Level, os.Getenv("LOGDOC_LOG_LEVEL"))
}

func setIf(dst *string, v string) {
	if v != "" {
		*dst = v
	}
}

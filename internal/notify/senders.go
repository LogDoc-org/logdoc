package notify

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/smtp"
	"strings"
)

// buildSenders instantiates every channel that has enough configuration.
func buildSenders(cfg Config) []Sender {
	var out []Sender
	if cfg.Telegram.Token != "" && len(cfg.Telegram.ChatIDs) > 0 {
		out = append(out, &Telegram{cfg: cfg.Telegram})
	}
	if cfg.Webhook.URL != "" {
		out = append(out, &Webhook{cfg: cfg.Webhook})
	}
	if cfg.Email.SMTPAddr != "" && cfg.Email.From != "" && len(cfg.Email.To) > 0 {
		out = append(out, &Email{cfg: cfg.Email})
	}
	if len(cfg.Kafka.Brokers) > 0 && cfg.Kafka.Topic != "" {
		out = append(out, newKafka(cfg.Kafka))
	}
	return out
}

// --- Telegram ---

type TelegramConfig struct {
	Token   string  `yaml:"token"`
	ChatIDs []int64 `yaml:"chat_ids"`
	// APIURL overrides the Bot API host (proxies, tests). Default: telegram.org.
	APIURL string `yaml:"api_url"`
}

// Telegram sends via the Bot API sendMessage method — a ported v1
// Telegramer pipe plugin without the bot library dependency.
type Telegram struct {
	cfg TelegramConfig
}

func (t *Telegram) Name() string { return "telegram" }

func (t *Telegram) Send(ctx context.Context, ev Event) error {
	base := t.cfg.APIURL
	if base == "" {
		base = "https://api.telegram.org"
	}
	url := fmt.Sprintf("%s/bot%s/sendMessage", strings.TrimSuffix(base, "/"), t.cfg.Token)
	for _, chat := range t.cfg.ChatIDs {
		body, _ := json.Marshal(map[string]any{
			"chat_id":                  chat,
			"text":                     "[LogDoc] " + ev.Message,
			"disable_web_page_preview": true,
		})
		if err := postJSON(ctx, url, body); err != nil {
			return fmt.Errorf("chat %d: %w", chat, err)
		}
	}
	return nil
}

// --- Webhook ---

type WebhookConfig struct {
	URL string `yaml:"url"`
}

// Webhook POSTs the raw event as JSON — the integration point for
// everything else (Slack proxies, incident tooling, scripts).
type Webhook struct {
	cfg WebhookConfig
}

func (w *Webhook) Name() string { return "webhook" }

func (w *Webhook) Send(ctx context.Context, ev Event) error {
	body, _ := json.Marshal(ev)
	return postJSON(ctx, w.cfg.URL, body)
}

// --- Email ---

type EmailConfig struct {
	SMTPAddr string   `yaml:"smtp_addr"` // host:port
	From     string   `yaml:"from"`
	To       []string `yaml:"to"`
	Username string   `yaml:"username"` // empty = no auth
	Password string   `yaml:"password"`
}

// Email sends a plain-text message over SMTP (STARTTLS when the server
// offers it) — a ported v1 Emailer pipe plugin.
type Email struct {
	cfg EmailConfig
}

func (e *Email) Name() string { return "email" }

func (e *Email) Send(_ context.Context, ev Event) error {
	var auth smtp.Auth
	if e.cfg.Username != "" {
		host := e.cfg.SMTPAddr
		if i := strings.LastIndex(host, ":"); i >= 0 {
			host = host[:i]
		}
		auth = smtp.PlainAuth("", e.cfg.Username, e.cfg.Password, host)
	}
	return smtp.SendMail(e.cfg.SMTPAddr, auth, e.cfg.From, e.cfg.To, emailBody(e.cfg.From, e.cfg.To, ev))
}

func emailBody(from string, to []string, ev Event) []byte {
	var b bytes.Buffer
	fmt.Fprintf(&b, "From: %s\r\n", from)
	fmt.Fprintf(&b, "To: %s\r\n", strings.Join(to, ", "))
	fmt.Fprintf(&b, "Subject: [LogDoc] %s\r\n", ev.Rule)
	b.WriteString("\r\n")
	b.WriteString(ev.Message)
	fmt.Fprintf(&b, "\r\n\r\nfired at %s\r\n", ev.Ts.Format("2006-01-02 15:04:05 MST"))
	return b.Bytes()
}

// postJSON POSTs a body and treats any non-2xx as an error.
func postJSON(ctx context.Context, url string, body []byte) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, url, bytes.NewReader(body))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")
	res, err := http.DefaultClient.Do(req)
	if err != nil {
		return err
	}
	defer func() { _ = res.Body.Close() }()
	if res.StatusCode < 200 || res.StatusCode > 299 {
		msg, _ := io.ReadAll(io.LimitReader(res.Body, 512))
		return fmt.Errorf("HTTP %d: %s", res.StatusCode, strings.TrimSpace(string(msg)))
	}
	return nil
}

package ingest

import (
	"bufio"
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"strings"
	"time"

	"github.com/LogDoc-org/logdoc/internal/model"
)

// Парсер v1-протокола ld_format (см. sdk/ld_format.md + реальные аппендеры).
//
// Событие: [0x06 0x03] пары... '\n'
// Пара текстовая:  KEY '=' VALUE '\n'
// Пара бинарная:   KEY '\n' be-uint32(len) VALUE ['\n' — опционален:
//                  спека требует, реальные аппендеры (Go/Java) не пишут]
//
// Границы события: '\n' в позиции начала пары, заголовок 0x06 0x03
// в позиции начала пары или EOF.

var ldHeader = [2]byte{6, 3}

const (
	maxKeyLen   = 256
	maxValueLen = 16 << 20 // 16 MiB на значение — защита от мусорной длины
)

// ldEvent — сырое распарсенное событие: ключ → значение.
type ldEvent map[string]string

// ParseLDStream читает события из потока до EOF, вызывая emit для каждого.
// События без msg игнорируются по спеке (но парсятся для сохранения границ).
func ParseLDStream(r *bufio.Reader, emit func(ldEvent)) error {
	for {
		ev, err := parseLDEvent(r)
		if ev != nil {
			emit(ev)
		}
		if err != nil {
			if errors.Is(err, io.EOF) {
				return nil
			}
			return err
		}
	}
}

// parseLDEvent читает одно событие. Возвращает событие (может быть nil,
// если границу встретили до первой пары) и ошибку (io.EOF — конец потока).
func parseLDEvent(r *bufio.Reader) (ldEvent, error) {
	var ev ldEvent

	skipHeader(r)

	for {
		// Позиция начала пары: проверяем границы события.
		b, err := r.Peek(1)
		if err != nil {
			return ev, io.EOF
		}
		if b[0] == '\n' { // пустая строка = конец события
			_, _ = r.Discard(1)
			return ev, nil
		}
		if b[0] == ldHeader[0] { // возможно, начало следующего события
			if h, err := r.Peek(2); err == nil && h[1] == ldHeader[1] {
				return ev, nil // заголовок не съедаем — его обработает следующий вызов
			}
		}

		key, delim, err := readKey(r)
		if err != nil {
			return ev, err
		}

		var value string
		if delim == '=' {
			raw, err := r.ReadString('\n')
			if err != nil {
				return ev, fmt.Errorf("незавершённая текстовая пара %q: %w", key, err)
			}
			value = strings.TrimSuffix(raw, "\n")
		} else { // бинарная пара
			var lenBuf [4]byte
			if _, err := io.ReadFull(r, lenBuf[:]); err != nil {
				return ev, fmt.Errorf("длина бинарной пары %q: %w", key, err)
			}
			n := binary.BigEndian.Uint32(lenBuf[:])
			if n > maxValueLen {
				return ev, fmt.Errorf("бинарная пара %q: длина %d превышает лимит", key, n)
			}
			buf := make([]byte, n)
			if _, err := io.ReadFull(r, buf); err != nil {
				return ev, fmt.Errorf("значение бинарной пары %q: %w", key, err)
			}
			value = string(buf)
			// Опциональный хвостовой '\n' (спека) — съедаем, только если за ним
			// не следует само значение границы: аппендеры хвост не пишут, и '\n'
			// после значения у них означает конец события. Отличить нельзя,
			// поэтому съедаем '\n' и полагаемся на границу по заголовку/EOF.
			if b, err := r.Peek(1); err == nil && b[0] == '\n' {
				_, _ = r.Discard(1)
			}
		}

		if ev == nil {
			ev = make(ldEvent, 8)
		}
		ev[key] = value
	}
}

func skipHeader(r *bufio.Reader) {
	if h, err := r.Peek(2); err == nil && h[0] == ldHeader[0] && h[1] == ldHeader[1] {
		_, _ = r.Discard(2)
	}
}

// readKey читает ключ до '=' (текстовая пара) или '\n' (бинарная).
// Валидация по спеке: непустой, ASCII, без управляющих символов.
func readKey(r *bufio.Reader) (string, byte, error) {
	var sb strings.Builder
	for {
		c, err := r.ReadByte()
		if err != nil {
			return "", 0, fmt.Errorf("незавершённый ключ %q: %w", sb.String(), err)
		}
		if c == '=' || c == '\n' {
			if sb.Len() == 0 {
				return "", 0, errors.New("пустой ключ")
			}
			return sb.String(), c, nil
		}
		if c < 0x20 || c > 0x7e {
			return "", 0, fmt.Errorf("недопустимый байт 0x%02x в ключе %q", c, sb.String())
		}
		if sb.Len() >= maxKeyLen {
			return "", 0, errors.New("ключ превышает лимит длины")
		}
		sb.WriteByte(c)
	}
}

// tsrc-форматы: канонический из спеки/Java и «кривой» из logdoc-go-appender
// (yy dd MM + точка перед миллисекундами).
var tsrcLayouts = []string{
	"060102150405000",  // yyMMddHHmmssSSS — спека и Java-аппендер
	"060201150405.000", // logdoc-go-appender
}

// EntryFromLD собирает model.Entry из сырого события.
// Возвращает false, если события нет или отсутствует обязательный msg.
func EntryFromLD(ev ldEvent, remoteIP string, now time.Time) (model.Entry, bool) {
	msg, ok := ev["msg"]
	if !ok || msg == "" {
		return model.Entry{}, false
	}

	e := model.Entry{
		TenantID: model.DefaultTenant,
		Ts:       now,
		Msg:      msg,
		App:      ev["app"],
		Src:      strings.TrimSpace(ev["src"]),
		PID:      strings.TrimSpace(ev["pid"]),
		Lvl:      parseLDLevel(ev["lvl"]),
	}

	if raw, ok := ev["tsrc"]; ok {
		if ts, ok := parseTsrc(strings.TrimSpace(raw)); ok {
			e.Ts = ts
		}
	}

	for k, v := range ev {
		switch k {
		case "msg", "app", "src", "pid", "lvl", "tsrc", "trcv", "ip":
			// служебные ключи: trcv/ip клиента перезаписываются сервером (спека)
		default:
			if e.Fields == nil {
				e.Fields = make(map[string]string)
			}
			e.Fields[k] = v
		}
	}
	if remoteIP != "" {
		if e.Fields == nil {
			e.Fields = make(map[string]string)
		}
		e.Fields["ip"] = remoteIP
	}
	return e, true
}

func parseTsrc(s string) (time.Time, bool) {
	for _, layout := range tsrcLayouts {
		if t, err := time.ParseInLocation(layout, s, time.Local); err == nil {
			return t, true
		}
	}
	return time.Time{}, false
}

// parseLDLevel принимает байт-цифру 0–6 (спека) или имя уровня:
// Java шлёт DEBUG/INFO/LOG/WARN/ERROR, logrus — lowercase + warning/fatal/trace.
func parseLDLevel(s string) model.Level {
	s = strings.TrimSpace(s)
	if s == "" {
		return model.LevelInfo
	}
	if len(s) == 1 && s[0] >= '0' && s[0] <= '6' {
		return model.Level(s[0] - '0')
	}
	switch strings.ToUpper(s) {
	case "WARNING":
		return model.LevelWarn
	case "FATAL":
		return model.LevelSevere
	case "TRACE":
		return model.LevelDebug
	}
	return model.ParseLevel(s)
}

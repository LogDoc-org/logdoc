package clickhouse

import (
	"context"
	"fmt"
	"log/slog"
	"strings"
	"time"

	"github.com/LogDoc-org/logdoc/internal/model"
	"github.com/LogDoc-org/logdoc/internal/query"
)

// BuildSQL транслирует логический план в SQL ClickHouse.
// Отдельная чистая функция — покрыта golden-тестами.
func BuildSQL(db string, p query.Plan) (string, []any) {
	var sb strings.Builder
	var args []any

	fmt.Fprintf(&sb, "SELECT tenant_id, app, src, lvl, pid, ts, msg, fields FROM %s.logs WHERE tenant_id = ?", db)
	args = append(args, p.TenantID)

	if len(p.Apps) > 0 {
		sb.WriteString(" AND app IN (" + placeholders(len(p.Apps)) + ")")
		for _, a := range p.Apps {
			args = append(args, a)
		}
	}
	if len(p.Levels) > 0 {
		sb.WriteString(" AND lvl IN (" + placeholders(len(p.Levels)) + ")")
		for _, l := range p.Levels {
			args = append(args, uint8(l))
		}
	}
	if p.From != nil {
		sb.WriteString(" AND ts >= ?")
		args = append(args, *p.From)
	}
	if p.To != nil {
		sb.WriteString(" AND ts <= ?")
		args = append(args, *p.To)
	}
	for _, k := range sortedKeys(p.FieldEq) {
		sb.WriteString(" AND fields[?] = ?")
		args = append(args, k, p.FieldEq[k])
	}
	if p.Search != "" {
		sb.WriteString(" AND positionCaseInsensitiveUTF8(msg, ?) > 0")
		args = append(args, p.Search)
	}

	limit := p.Limit
	if limit <= 0 {
		limit = query.DefaultLimit
	}
	fmt.Fprintf(&sb, " ORDER BY ts DESC LIMIT %d", limit)

	return sb.String(), args
}

func placeholders(n int) string {
	return strings.TrimSuffix(strings.Repeat("?,", n), ",")
}

func sortedKeys(m map[string]string) []string {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	// детерминированный порядок — для golden-тестов и кэшируемости SQL
	for i := 1; i < len(keys); i++ {
		for j := i; j > 0 && keys[j] < keys[j-1]; j-- {
			keys[j], keys[j-1] = keys[j-1], keys[j]
		}
	}
	return keys
}

// Query исполняет логический план.
func (s *Store) Query(ctx context.Context, p query.Plan) ([]model.Entry, error) {
	sql, args := BuildSQL(s.db, p)

	rows, err := s.conn.Query(ctx, sql, args...)
	if err != nil {
		return nil, fmt.Errorf("clickhouse query: %w", err)
	}
	defer rows.Close()

	var out []model.Entry
	for rows.Next() {
		var (
			e   model.Entry
			lvl uint8
		)
		if err := rows.Scan(&e.TenantID, &e.App, &e.Src, &lvl, &e.PID, &e.Ts, &e.Msg, &e.Fields); err != nil {
			return nil, fmt.Errorf("clickhouse scan: %w", err)
		}
		e.Lvl = model.Level(lvl)
		out = append(out, e)
	}
	return out, rows.Err()
}

// RecordQueryStats пишет workload telemetry (fire-and-forget: ошибка не
// влияет на ответ пользователю, только логируется).
func (s *Store) RecordQueryStats(ctx context.Context, st query.Stats) {
	ctx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	err := s.conn.Exec(ctx,
		fmt.Sprintf(`INSERT INTO %s.query_telemetry (ts, tenant_id, plan, duration_ms, rows) VALUES (?, ?, ?, ?, ?)`, s.db),
		st.Ts, st.TenantID, st.PlanJSON, uint32(st.DurationMs), uint32(st.Rows)) //nolint:gosec
	if err != nil {
		slog.Warn("query telemetry insert failed", "err", err)
	}
}

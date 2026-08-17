package clickhouse

import (
	"reflect"
	"testing"
	"time"

	"github.com/LogDoc-org/logdoc/internal/model"
	"github.com/LogDoc-org/logdoc/internal/query"
)

// Golden-тесты: точный SQL для каждого варианта плана.
// Ломаются при любом изменении генератора — это сознательно:
// SQL-диалект — контракт бэкенда.

func TestBuildSQLTenantOnly(t *testing.T) {
	sql, args := BuildSQL("logdoc", query.Plan{TenantID: "default", Limit: 100})
	wantSQL := "SELECT tenant_id, app, src, lvl, pid, ts, msg, fields FROM logdoc.logs WHERE tenant_id = ? ORDER BY ts DESC LIMIT 100"
	if sql != wantSQL {
		t.Fatalf("SQL:\n got: %s\nwant: %s", sql, wantSQL)
	}
	if !reflect.DeepEqual(args, []any{"default"}) {
		t.Fatalf("args: %v", args)
	}
}

func TestBuildSQLFullPlan(t *testing.T) {
	from := time.Date(2026, 8, 17, 0, 0, 0, 0, time.UTC)
	to := time.Date(2026, 8, 18, 0, 0, 0, 0, time.UTC)
	p := query.Plan{
		TenantID: "default",
		Apps:     []string{"svc1", "svc2"},
		Levels:   []model.Level{model.LevelError, model.LevelSevere},
		From:     &from,
		To:       &to,
		FieldEq:  map[string]string{"user": "u1", "env": "prod"},
		Search:   "timeout",
		Limit:    500,
	}
	sql, args := BuildSQL("logdoc", p)

	wantSQL := "SELECT tenant_id, app, src, lvl, pid, ts, msg, fields FROM logdoc.logs" +
		" WHERE tenant_id = ?" +
		" AND app IN (?,?)" +
		" AND lvl IN (?,?)" +
		" AND ts >= ? AND ts <= ?" +
		" AND fields[?] = ? AND fields[?] = ?" +
		" AND positionCaseInsensitiveUTF8(msg, ?) > 0" +
		" ORDER BY ts DESC LIMIT 500"
	if sql != wantSQL {
		t.Fatalf("SQL:\n got: %s\nwant: %s", sql, wantSQL)
	}

	wantArgs := []any{
		"default", "svc1", "svc2", uint8(4), uint8(5), from, to,
		"env", "prod", "user", "u1", // ключи fields — отсортированы
		"timeout",
	}
	if !reflect.DeepEqual(args, wantArgs) {
		t.Fatalf("args:\n got: %#v\nwant: %#v", args, wantArgs)
	}
}

func TestBuildSQLDefaultLimit(t *testing.T) {
	sql, _ := BuildSQL("logdoc", query.Plan{TenantID: "default"})
	want := " ORDER BY ts DESC LIMIT 100"
	if len(sql) < len(want) || sql[len(sql)-len(want):] != want {
		t.Fatalf("нет дефолтного лимита: %s", sql)
	}
}

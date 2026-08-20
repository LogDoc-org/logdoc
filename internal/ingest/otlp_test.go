package ingest

import (
	"context"
	"testing"
	"time"

	collogspb "go.opentelemetry.io/proto/otlp/collector/logs/v1"
	commonpb "go.opentelemetry.io/proto/otlp/common/v1"
	logspb "go.opentelemetry.io/proto/otlp/logs/v1"
	resourcepb "go.opentelemetry.io/proto/otlp/resource/v1"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/credentials/insecure"
	"google.golang.org/grpc/metadata"
	"google.golang.org/grpc/status"

	"github.com/LogDoc-org/logdoc/internal/model"
)

func strVal(s string) *commonpb.AnyValue {
	return &commonpb.AnyValue{Value: &commonpb.AnyValue_StringValue{StringValue: s}}
}

func kv(k, v string) *commonpb.KeyValue {
	return &commonpb.KeyValue{Key: k, Value: strVal(v)}
}

func exportReq() *collogspb.ExportLogsServiceRequest {
	ts := time.Date(2026, 8, 17, 12, 0, 0, 500e6, time.UTC)
	return &collogspb.ExportLogsServiceRequest{
		ResourceLogs: []*logspb.ResourceLogs{{
			Resource: &resourcepb.Resource{
				Attributes: []*commonpb.KeyValue{
					kv("service.name", "payment"),
					kv("host.name", "node-1"),
				},
			},
			ScopeLogs: []*logspb.ScopeLogs{{
				Scope: &commonpb.InstrumentationScope{Name: "billing/db"},
				LogRecords: []*logspb.LogRecord{
					{
						TimeUnixNano:   uint64(ts.UnixNano()),
						SeverityNumber: logspb.SeverityNumber_SEVERITY_NUMBER_ERROR,
						Body:           strVal("insert failed"),
						Attributes: []*commonpb.KeyValue{
							kv("db", "orders"),
							{Key: "retries", Value: &commonpb.AnyValue{Value: &commonpb.AnyValue_IntValue{IntValue: 3}}},
						},
						TraceId: []byte{1, 2, 3, 4, 5, 6, 7, 8, 9, 10, 11, 12, 13, 14, 15, 16},
						SpanId:  []byte{1, 2, 3, 4, 5, 6, 7, 8},
					},
					{Body: strVal("")}, // empty body → rejected
				},
			}},
		}},
	}
}

func TestOTLPExportMapping(t *testing.T) {
	sa := &syncAppender{}
	svc := &logsService{app: sa}

	resp, err := svc.Export(context.Background(), exportReq())
	if err != nil {
		t.Fatal(err)
	}
	if got := resp.GetPartialSuccess().GetRejectedLogRecords(); got != 1 {
		t.Fatalf("rejected = %d, want 1", got)
	}
	waitCount(t, sa, 1)

	e := sa.get(0)
	if e.App != "payment" || e.Src != "billing/db" || e.Lvl != model.LevelError {
		t.Fatalf("mapping: %+v", e)
	}
	if e.Msg != "insert failed" {
		t.Fatalf("msg = %q", e.Msg)
	}
	if e.TenantID != model.DefaultTenant {
		t.Fatalf("tenant = %q", e.TenantID)
	}
	if !e.Ts.Equal(time.Date(2026, 8, 17, 12, 0, 0, 500e6, time.UTC)) {
		t.Fatalf("ts = %v", e.Ts)
	}
	want := map[string]string{
		"host.name": "node-1",
		"db":        "orders",
		"retries":   "3",
		"trace_id":  "0102030405060708090a0b0c0d0e0f10",
		"span_id":   "0102030405060708",
	}
	for k, v := range want {
		if e.Fields[k] != v {
			t.Fatalf("fields[%s] = %q, want %q (all: %v)", k, e.Fields[k], v, e.Fields)
		}
	}
	if _, ok := e.Fields["service.name"]; ok {
		t.Fatal("service.name must map to app, not stay in fields")
	}
}

func TestOTLPSeverityMapping(t *testing.T) {
	cases := []struct {
		n    logspb.SeverityNumber
		text string
		want model.Level
	}{
		{logspb.SeverityNumber_SEVERITY_NUMBER_TRACE, "", model.LevelDebug},
		{logspb.SeverityNumber_SEVERITY_NUMBER_DEBUG4, "", model.LevelDebug},
		{logspb.SeverityNumber_SEVERITY_NUMBER_INFO, "", model.LevelInfo},
		{logspb.SeverityNumber_SEVERITY_NUMBER_WARN2, "", model.LevelWarn},
		{logspb.SeverityNumber_SEVERITY_NUMBER_ERROR, "", model.LevelError},
		{logspb.SeverityNumber_SEVERITY_NUMBER_FATAL, "", model.LevelSevere},
		{logspb.SeverityNumber_SEVERITY_NUMBER_UNSPECIFIED, "warn", model.LevelWarn},
		{logspb.SeverityNumber_SEVERITY_NUMBER_UNSPECIFIED, "", model.LevelInfo},
	}
	for _, c := range cases {
		if got := lvlFromOTLP(c.n, c.text); got != c.want {
			t.Fatalf("lvlFromOTLP(%d, %q) = %v, want %v", c.n, c.text, got, c.want)
		}
	}
}

func TestOTLPAnyValueRendering(t *testing.T) {
	arr := &commonpb.AnyValue{Value: &commonpb.AnyValue_ArrayValue{ArrayValue: &commonpb.ArrayValue{
		Values: []*commonpb.AnyValue{strVal("a"), {Value: &commonpb.AnyValue_BoolValue{BoolValue: true}}},
	}}}
	if got := anyValueString(arr); got != "[a,true]" {
		t.Fatalf("array = %q", got)
	}
	kvl := &commonpb.AnyValue{Value: &commonpb.AnyValue_KvlistValue{KvlistValue: &commonpb.KeyValueList{
		Values: []*commonpb.KeyValue{kv("x", "1")},
	}}}
	if got := anyValueString(kvl); got != "{x=1}" {
		t.Fatalf("kvlist = %q", got)
	}
	dbl := &commonpb.AnyValue{Value: &commonpb.AnyValue_DoubleValue{DoubleValue: 1.5}}
	if got := anyValueString(dbl); got != "1.5" {
		t.Fatalf("double = %q", got)
	}
}

// TestOTLPGRPCEndToEnd starts a real gRPC server and pushes logs through
// the wire, including the API-key check.
func TestOTLPGRPCEndToEnd(t *testing.T) {
	sa := &syncAppender{}
	srv, err := StartOTLP(sa, "127.0.0.1:0", func(cred string) bool { return cred == "sekret" })
	if err != nil {
		t.Fatal(err)
	}
	defer func() {
		ctx, cancel := context.WithTimeout(context.Background(), time.Second)
		defer cancel()
		srv.Shutdown(ctx)
	}()

	conn, err := grpc.NewClient(srv.Addr(), grpc.WithTransportCredentials(insecure.NewCredentials()))
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = conn.Close() }()
	client := collogspb.NewLogsServiceClient(conn)

	// Without a key → Unauthenticated.
	_, err = client.Export(context.Background(), exportReq())
	if status.Code(err) != codes.Unauthenticated {
		t.Fatalf("no key: err = %v, want Unauthenticated", err)
	}

	// With the key → accepted.
	ctx := metadata.AppendToOutgoingContext(context.Background(), "x-api-key", "sekret")
	if _, err := client.Export(ctx, exportReq()); err != nil {
		t.Fatal(err)
	}
	waitCount(t, sa, 1)
	if e := sa.get(0); e.App != "payment" {
		t.Fatalf("app = %q", e.App)
	}
}

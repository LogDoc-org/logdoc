// OTLP/gRPC logs intake (S2): the logs service only, mapped into model.Entry.
// Mapping: body→msg, severity_number→lvl, time_unix_nano→ts,
// resource service.name→app, scope name→src, attributes→fields,
// trace_id/span_id→fields (hex) — the topology extractor feeds on those.
package ingest

import (
	"context"
	"crypto/subtle"
	"encoding/hex"
	"fmt"
	"net"
	"strconv"
	"strings"
	"time"

	collogspb "go.opentelemetry.io/proto/otlp/collector/logs/v1"
	commonpb "go.opentelemetry.io/proto/otlp/common/v1"
	logspb "go.opentelemetry.io/proto/otlp/logs/v1"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/metadata"
	"google.golang.org/grpc/status"

	"github.com/LogDoc-org/logdoc/internal/model"
)

// OTLPServer — gRPC server with the OTLP LogsService registered.
type OTLPServer struct {
	srv *grpc.Server
	ln  net.Listener
}

type logsService struct {
	collogspb.UnimplementedLogsServiceServer
	app Appender
}

// StartOTLP starts the OTLP/gRPC listener. An empty addr disables it.
// A non-empty apiKey is required from clients via the x-api-key or
// authorization (Bearer) metadata.
func StartOTLP(app Appender, addr, apiKey string) (*OTLPServer, error) {
	if addr == "" {
		return &OTLPServer{}, nil
	}
	ln, err := net.Listen("tcp", addr)
	if err != nil {
		return nil, fmt.Errorf("otlp grpc listen %s: %w", addr, err)
	}
	srv := grpc.NewServer(grpc.UnaryInterceptor(apiKeyUnaryInterceptor(apiKey)))
	collogspb.RegisterLogsServiceServer(srv, &logsService{app: app})
	go func() { _ = srv.Serve(ln) }()
	return &OTLPServer{srv: srv, ln: ln}, nil
}

// Addr returns the actual listen address (useful with ":0" in tests).
func (s *OTLPServer) Addr() string {
	if s.ln == nil {
		return ""
	}
	return s.ln.Addr().String()
}

// Shutdown stops the server gracefully within the context deadline.
func (s *OTLPServer) Shutdown(ctx context.Context) {
	if s.srv == nil {
		return
	}
	done := make(chan struct{})
	go func() {
		s.srv.GracefulStop()
		close(done)
	}()
	select {
	case <-done:
	case <-ctx.Done():
		s.srv.Stop()
	}
}

func apiKeyUnaryInterceptor(key string) grpc.UnaryServerInterceptor {
	return func(ctx context.Context, req any, _ *grpc.UnaryServerInfo, handler grpc.UnaryHandler) (any, error) {
		if key == "" {
			return handler(ctx, req)
		}
		md, _ := metadata.FromIncomingContext(ctx)
		got := firstMD(md, "x-api-key")
		if got == "" {
			if auth := firstMD(md, "authorization"); strings.HasPrefix(auth, "Bearer ") {
				got = strings.TrimPrefix(auth, "Bearer ")
			}
		}
		if subtle.ConstantTimeCompare([]byte(got), []byte(key)) != 1 {
			return nil, status.Error(codes.Unauthenticated, "invalid API key")
		}
		return handler(ctx, req)
	}
}

func firstMD(md metadata.MD, key string) string {
	if vs := md.Get(key); len(vs) > 0 {
		return vs[0]
	}
	return ""
}

func (s *logsService) Export(_ context.Context, req *collogspb.ExportLogsServiceRequest) (*collogspb.ExportLogsServiceResponse, error) {
	now := time.Now()
	var rejected int64
	for _, rl := range req.GetResourceLogs() {
		app, resFields := resourceInfo(rl.GetResource().GetAttributes())
		for _, sl := range rl.GetScopeLogs() {
			src := sl.GetScope().GetName()
			for _, rec := range sl.GetLogRecords() {
				e := entryFromOTLP(rec, app, src, resFields, now)
				if e.Msg == "" {
					rejected++ // msg is mandatory across all LogDoc ingest paths
					continue
				}
				s.app.Append(e)
			}
		}
	}
	resp := &collogspb.ExportLogsServiceResponse{}
	if rejected > 0 {
		resp.PartialSuccess = &collogspb.ExportLogsPartialSuccess{
			RejectedLogRecords: rejected,
			ErrorMessage:       "records with an empty body were rejected (msg is mandatory)",
		}
	}
	return resp, nil
}

// resourceInfo extracts the app name (service.name) and the remaining
// resource attributes shared by all records of this resource.
func resourceInfo(attrs []*commonpb.KeyValue) (app string, fields map[string]string) {
	for _, kv := range attrs {
		v := anyValueString(kv.GetValue())
		if kv.GetKey() == "service.name" {
			app = v
			continue
		}
		if fields == nil {
			fields = make(map[string]string)
		}
		fields[kv.GetKey()] = v
	}
	return app, fields
}

func entryFromOTLP(rec *logspb.LogRecord, app, src string, resFields map[string]string, now time.Time) model.Entry {
	e := model.Entry{
		TenantID: model.DefaultTenant,
		App:      app,
		Src:      src,
		Lvl:      lvlFromOTLP(rec.GetSeverityNumber(), rec.GetSeverityText()),
		Msg:      anyValueString(rec.GetBody()),
	}

	switch {
	case rec.GetTimeUnixNano() != 0:
		e.Ts = time.Unix(0, int64(rec.GetTimeUnixNano()))
	case rec.GetObservedTimeUnixNano() != 0:
		e.Ts = time.Unix(0, int64(rec.GetObservedTimeUnixNano()))
	default:
		e.Ts = now
	}

	n := len(resFields) + len(rec.GetAttributes()) + 2
	if n > 0 {
		e.Fields = make(map[string]string, n)
	}
	for k, v := range resFields {
		e.Fields[k] = v
	}
	// Record attributes take precedence over resource attributes.
	for _, kv := range rec.GetAttributes() {
		e.Fields[kv.GetKey()] = anyValueString(kv.GetValue())
	}
	if tid := rec.GetTraceId(); !allZero(tid) {
		e.Fields["trace_id"] = hex.EncodeToString(tid)
	}
	if sid := rec.GetSpanId(); !allZero(sid) {
		e.Fields["span_id"] = hex.EncodeToString(sid)
	}
	return e
}

// lvlFromOTLP maps the OTLP severity_number ranges (1-24) onto LogDoc levels;
// with an unspecified number it falls back to parsing severity_text.
func lvlFromOTLP(n logspb.SeverityNumber, text string) model.Level {
	switch {
	case n >= logspb.SeverityNumber_SEVERITY_NUMBER_FATAL: // 21-24
		return model.LevelSevere
	case n >= logspb.SeverityNumber_SEVERITY_NUMBER_ERROR: // 17-20
		return model.LevelError
	case n >= logspb.SeverityNumber_SEVERITY_NUMBER_WARN: // 13-16
		return model.LevelWarn
	case n >= logspb.SeverityNumber_SEVERITY_NUMBER_INFO: // 9-12
		return model.LevelInfo
	case n >= logspb.SeverityNumber_SEVERITY_NUMBER_TRACE: // 1-8 (trace+debug)
		return model.LevelDebug
	}
	if text != "" {
		return model.ParseLevel(text)
	}
	return model.LevelInfo
}

// anyValueString renders an OTLP AnyValue as a string (log values are
// stored as strings in S1/S2; typed fields come later).
func anyValueString(v *commonpb.AnyValue) string {
	switch val := v.GetValue().(type) {
	case *commonpb.AnyValue_StringValue:
		return val.StringValue
	case *commonpb.AnyValue_BoolValue:
		return strconv.FormatBool(val.BoolValue)
	case *commonpb.AnyValue_IntValue:
		return strconv.FormatInt(val.IntValue, 10)
	case *commonpb.AnyValue_DoubleValue:
		return strconv.FormatFloat(val.DoubleValue, 'g', -1, 64)
	case *commonpb.AnyValue_BytesValue:
		return hex.EncodeToString(val.BytesValue)
	case *commonpb.AnyValue_ArrayValue:
		parts := make([]string, 0, len(val.ArrayValue.GetValues()))
		for _, item := range val.ArrayValue.GetValues() {
			parts = append(parts, anyValueString(item))
		}
		return "[" + strings.Join(parts, ",") + "]"
	case *commonpb.AnyValue_KvlistValue:
		parts := make([]string, 0, len(val.KvlistValue.GetValues()))
		for _, kv := range val.KvlistValue.GetValues() {
			parts = append(parts, kv.GetKey()+"="+anyValueString(kv.GetValue()))
		}
		return "{" + strings.Join(parts, ",") + "}"
	default:
		return ""
	}
}

func allZero(b []byte) bool {
	for _, x := range b {
		if x != 0 {
			return false
		}
	}
	return true
}

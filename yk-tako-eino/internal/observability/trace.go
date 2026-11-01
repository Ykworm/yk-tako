// Package observability assembles the OTel GenAI trace pipeline (doc/54).
// Missing credentials = no-op: local dev and unit tests never touch the network.
package observability

import (
	"context"
	"fmt"
	"log"
	"os"
	"strings"
	"sync"
	"time"

	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/exporters/otlp/otlptrace/otlptracehttp"
	"go.opentelemetry.io/otel/sdk/resource"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
	semconv "go.opentelemetry.io/otel/semconv/v1.24.0"
	"go.opentelemetry.io/otel/trace"
)

const (
	cozeLoopEndpoint = "api.coze.cn"
	cozeLoopPath     = "/v1/loop/opentelemetry/v1/traces"
	captureEnv       = "OTEL_INSTRUMENTATION_GENAI_CAPTURE_CONTENT"
	serviceName      = "yktako"
)

var (
	mu       sync.RWMutex
	provider *sdktrace.TracerProvider
	enabled  bool
	capture  bool
)

// Setup builds the tracer provider. It never fails startup: a broken exporter or
// missing credentials only disable tracing. Must be called once before workers start.
func Setup(ctx context.Context) {
	token := strings.TrimSpace(os.Getenv("COZELOOP_API_TOKEN"))
	workspace := strings.TrimSpace(os.Getenv("COZELOOP_WORKSPACE_ID"))
	capture = parseCapture(os.Getenv(captureEnv))

	mu.Lock()
	defer mu.Unlock()
	if token == "" || workspace == "" {
		enabled = false
		provider = nil
		log.Printf("trace: disabled (COZELOOP_* 未配置)")
		log.Printf("trace content: %s", onOff(capture))
		return
	}
	exp, err := otlptracehttp.New(ctx,
		otlptracehttp.WithEndpoint(cozeLoopEndpoint),
		otlptracehttp.WithURLPath(cozeLoopPath),
		otlptracehttp.WithHeaders(map[string]string{
			"authorization":         "Bearer " + token,
			"cozeloop-workspace-id": workspace,
		}),
		otlptracehttp.WithTimeout(5*time.Second),
	)
	if err != nil {
		enabled = false
		log.Printf("trace: disabled (exporter: %v)", err)
		return
	}
	host, _ := os.Hostname()
	res, err := resource.New(ctx, resource.WithAttributes(
		semconv.ServiceName(serviceName),
		semconv.ServiceInstanceID(fmt.Sprintf("%s:%d", host, os.Getpid())),
	))
	if err != nil {
		res = resource.Default()
	}
	provider = sdktrace.NewTracerProvider(
		sdktrace.WithBatcher(exp),
		sdktrace.WithResource(res),
	)
	otel.SetTracerProvider(provider)
	otel.SetErrorHandler(otel.ErrorHandlerFunc(func(err error) {
		log.Printf("trace export: %v", err)
	}))
	enabled = true
	log.Printf("trace: enabled (workspace=%s)", workspace)
	log.Printf("trace content: %s", onOff(capture))
}

// Ready reports whether an exporter is configured.
func Ready() bool {
	mu.RLock()
	defer mu.RUnlock()
	return enabled
}

// CaptureContent reports whether user/model content may be written onto spans.
// Unset or unknown values fall back to NO_CONTENT per the OTel GenAI spec.
func CaptureContent() bool {
	mu.RLock()
	defer mu.RUnlock()
	return capture
}

// Tracer returns the global tracer. When Setup disabled the pipeline this is the
// no-op tracer, so callers never branch on Ready just to start a span.
func Tracer() trace.Tracer {
	return otel.Tracer(serviceName)
}

// ForceFlush exports buffered spans. Safe to call when tracing is disabled.
func ForceFlush(ctx context.Context) error {
	mu.RLock()
	p := provider
	mu.RUnlock()
	if p == nil {
		return nil
	}
	return p.ForceFlush(ctx)
}

func parseCapture(raw string) bool {
	switch strings.ToUpper(strings.TrimSpace(raw)) {
	case "SPAN_ONLY", "SPAN_AND_EVENT":
		return true
	default:
		// NO_CONTENT, EVENT_ONLY (we emit no events), unset and unknown values.
		return false
	}
}

func onOff(v bool) string {
	if v {
		return "on"
	}
	return "off"
}

package runengine

import (
	"context"
	"net/http"
	"time"

	"yk-tako-eino/internal/observability"

	"github.com/cloudwego/eino/schema"
	"github.com/gin-gonic/gin"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/codes"
	"go.opentelemetry.io/otel/trace"
)

// runUsage is the conversation ledger (doc/54 §2.3): only `turn` entries count,
// which already include root and sub-agent calls; compaction summaries and memory
// extraction run outside this slice.
type runUsage struct {
	Input     int
	Output    int
	Turns     int
	ToolCalls int
}

func sumRunUsage(turn []TurnEntry) runUsage {
	var u runUsage
	for _, e := range turn {
		if e.Role != "assistant" {
			continue
		}
		u.Turns++
		u.ToolCalls += len(e.ToolCalls)
		if e.Usage == nil {
			continue
		}
		u.Input += e.Usage.PromptTokens
		u.Output += e.Usage.CompletionTokens
	}
	return u
}

// turnMessages converts assistant entries into the semconv message shape. rootOnly
// keeps only the main agent's entries for the run span's output attribute.
func turnMessages(turn []TurnEntry, rootOnly bool) []*schema.Message {
	var out []*schema.Message
	for _, e := range turn {
		if e.Role != "assistant" {
			continue
		}
		if rootOnly && e.Agent != "" {
			continue
		}
		m := &schema.Message{Role: schema.Assistant, Content: e.Text, ToolCalls: e.ToolCalls}
		out = append(out, m)
	}
	return out
}

// loadModelMessages wraps the mh read in the history step span. Both input paths
// (fresh run and resume) go through it, so the timeline can separate history load
// from recall and folding.
func (w *Worker) loadModelMessages(ctx context.Context, sessionID SessionID) ([]ModelMessage, error) {
	ctx, span := startStep(ctx, "tako.step.history")
	msgs, err := w.store.ModelMessages(ctx, sessionID)
	span.SetAttributes(
		attribute.Int("tako.history.messages", len(msgs)),
		attribute.Bool("tako.history.resume", historyResumeFrom(ctx)),
	)
	finishSpan(span, err)
	return msgs, err
}

// runStatus reads the terminal status written by EndRun; the stop endpoint flips it
// in the DB, so a local variable cannot stand in for it.
func (s *Store) runStatus(ctx context.Context, runID RunID) (string, error) {
	var status string
	err := s.db.WithContext(ctx).Model(&Run{}).Where("id = ?", runID).Select("status").Scan(&status).Error
	return status, err
}

// runUserText returns the turn's original user text (before recall injection is
// appended by UpdateRunUserText).
func (s *Store) runUserText(ctx context.Context, sessionID SessionID, runID RunID) (string, error) {
	var text string
	err := s.db.WithContext(ctx).Model(&RunlogEntry{}).
		Where("session_id = ? AND run_id = ? AND role = 'user'", sessionID, runID).
		Order("seq").Select("text").Limit(1).Scan(&text).Error
	return text, err
}

// finishRunSpan writes the closing attributes and status. It reads runs.status so a
// user stop (flipped by the stop endpoint) is reported as stopped, not complete.
func (w *Worker) finishRunSpan(ctx context.Context, span trace.Span, sessionID SessionID, runID RunID, localStatus string, turn []TurnEntry, staged int, modelRef string) {
	status := localStatus
	if got, err := w.store.runStatus(context.WithoutCancel(ctx), runID); err == nil && got != "" {
		status = got
	}
	u := sumRunUsage(turn)
	span.SetAttributes(
		attribute.String("tako.run.status", status),
		attribute.Int("tako.run.turns", u.Turns),
		attribute.Int("tako.run.tool_calls", u.ToolCalls),
		attribute.Int(attrInputTok, u.Input),
		attribute.Int(attrOutputTok, u.Output),
		attribute.Int(attrTotalTok, u.Input+u.Output),
		attribute.String("tako.usage.scope", "conversation"),
	)
	if modelRef == "" {
		for i := len(turn) - 1; i >= 0; i-- {
			if turn[i].Model != "" {
				modelRef = turn[i].Model
				break
			}
		}
	}
	if provider, name := splitModelRef(modelRef); name != "" {
		span.SetAttributes(attribute.String(attrModel, name))
		if provider != "" {
			span.SetAttributes(attribute.String(attrProvider, provider))
		}
	}
	if staged > 0 {
		span.SetAttributes(attribute.Bool("gen_ai.conversation.compacted", true))
	}
	observability.SetMessages(span, "gen_ai.output.messages", turnMessages(turn, true))
	switch status {
	case "error":
		span.SetAttributes(attribute.String("error.type", "run_error"))
		span.SetStatus(codes.Error, "run failed")
	case "complete":
		span.SetStatus(codes.Ok, "")
	default:
		span.SetStatus(codes.Unset, "")
	}
	span.End()
}

// traceIngress starts a SERVER span for the HTTP request and carries its context
// and start time into the worker, so the run span becomes its child and
// tako.run.start_delay_ms measures admission/scheduling latency (doc/54 §2.4).
func traceIngress() gin.HandlerFunc {
	return func(c *gin.Context) {
		start := time.Now()
		ctx, span := observability.Tracer().Start(c.Request.Context(),
			c.Request.Method+" "+c.FullPath(),
			trace.WithSpanKind(trace.SpanKindServer))
		span.SetAttributes(
			attribute.String("http.request.method", c.Request.Method),
			attribute.String("http.route", c.FullPath()),
			attribute.String("url.path", c.Request.URL.Path),
		)
		ctx = trace.ContextWithSpanContext(ctx, span.SpanContext())
		c.Request = c.Request.WithContext(withIngressStart(ctx, start))
		defer span.End()
		c.Next()
		status := c.Writer.Status()
		span.SetAttributes(attribute.Int("http.response.status_code", status))
		if status >= 500 {
			span.SetStatus(codes.Error, http.StatusText(status))
		}
	}
}

// launchContext rebuilds the background context used by workers: the request's ctx
// values (principal/profile) are applied later by the callers, but the trace
// parent and ingress start time must come from the HTTP request, not be lost.
func launchContext(reqCtx context.Context) context.Context {
	runCtx := context.Background()
	if sc := trace.SpanContextFromContext(reqCtx); sc.IsValid() {
		runCtx = trace.ContextWithSpanContext(runCtx, sc)
	}
	if t0, ok := ingressStartFrom(reqCtx); ok {
		runCtx = withIngressStart(runCtx, t0)
	}
	return runCtx
}

// devTraceCheck is the pipeline probe (doc/54 A1): it sends one sample root span
// and flushes, so credentials/endpoint/classification can be verified without a run.
func (s *Server) devTraceCheck(c *gin.Context) {
	if _, ok := s.requirePrincipal(c); !ok {
		return
	}
	if !observability.Ready() {
		writeEnvelope(c, http.StatusOK, codeOK, "", gin.H{"traceId": "", "exported": false, "reason": "disabled"})
		return
	}
	_, span := observability.Tracer().Start(c.Request.Context(), "invoke_agent yktako", trace.WithSpanKind(trace.SpanKindClient))
	span.SetAttributes(
		attribute.String(attrOperation, "invoke_agent"),
		attribute.String("gen_ai.agent.name", "yktako"),
		attribute.String(attrProvider, "deepseek"),
		attribute.String(attrModel, "trace-check"),
		attribute.String("gen_ai.conversation.id", "trace-check"),
		attribute.String("tako.run.id", "trace-check"),
		attribute.String("cozeloop.span_type", "agent"),
	)
	span.SetStatus(codes.Ok, "")
	traceID := span.SpanContext().TraceID().String()
	span.End()
	err := observability.ForceFlush(c.Request.Context())
	data := gin.H{"traceId": traceID, "exported": err == nil}
	if err != nil {
		data["reason"] = err.Error()
	}
	writeEnvelope(c, http.StatusOK, codeOK, "", data)
}

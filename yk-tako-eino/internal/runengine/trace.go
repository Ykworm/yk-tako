package runengine

import (
	"context"
	"errors"
	"io"
	"strings"
	"time"
	"unicode/utf8"

	"yk-tako-eino/internal/observability"

	"github.com/cloudwego/eino/components/model"
	"github.com/cloudwego/eino/schema"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/codes"
	"go.opentelemetry.io/otel/trace"
)

const (
	attrOperation   = "gen_ai.operation.name"
	attrProvider    = "gen_ai.provider.name"
	attrModel       = "gen_ai.request.model"
	attrResponseMdl = "gen_ai.response.model"
	attrInputTok    = "gen_ai.usage.input_tokens"
	attrOutputTok   = "gen_ai.usage.output_tokens"
	attrTotalTok    = "gen_ai.usage.total_tokens"
	attrFinish      = "gen_ai.response.finish_reasons"
	attrTTFT        = "gen_ai.response.time_to_first_chunk"
	attrTPOT        = "tako.chat.time_per_output_token"
	attrEstimated   = "tako.chat.output_tokens_estimated"
)

type ingressStartKey struct{}

func withIngressStart(ctx context.Context, t time.Time) context.Context {
	return context.WithValue(ctx, ingressStartKey{}, t)
}

func ingressStartFrom(ctx context.Context) (time.Time, bool) {
	t, ok := ctx.Value(ingressStartKey{}).(time.Time)
	return t, ok
}

type compactTriggerKey struct{}

func withCompactTrigger(ctx context.Context, trigger string) context.Context {
	return context.WithValue(ctx, compactTriggerKey{}, trigger)
}

func compactTriggerFrom(ctx context.Context) string {
	v, _ := ctx.Value(compactTriggerKey{}).(string)
	return v
}

type historyResumeKey struct{}

func withHistoryResume(ctx context.Context, resume bool) context.Context {
	return context.WithValue(ctx, historyResumeKey{}, resume)
}

func historyResumeFrom(ctx context.Context) bool {
	v, _ := ctx.Value(historyResumeKey{}).(bool)
	return v
}

func startStep(ctx context.Context, name string) (context.Context, trace.Span) {
	return observability.Tracer().Start(ctx, name, trace.WithSpanKind(trace.SpanKindInternal))
}

func finishSpan(span trace.Span, err error) {
	if err == nil {
		span.SetStatus(codes.Ok, "")
	} else if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
		span.SetStatus(codes.Unset, "")
	} else {
		span.RecordError(err)
		span.SetStatus(codes.Error, err.Error())
	}
	span.End()
}

// splitModelRef turns a model ref ("deepseek/deepseek-chat") into provider/model.
func splitModelRef(ref string) (string, string) {
	ref = strings.TrimSpace(ref)
	if i := strings.IndexByte(ref, '/'); i > 0 && i < len(ref)-1 {
		return ref[:i], ref[i+1:]
	}
	return "", ref
}

// wrapChatModel traces every model call. Already-traced models pass through so a
// model resolved by more than one layer does not produce nested chat spans.
func wrapChatModel(m model.BaseChatModel, ref string) model.BaseChatModel {
	if m == nil {
		return nil
	}
	if _, ok := m.(interface{ traced() }); ok {
		return m
	}
	provider, name := splitModelRef(ref)
	return &tracedChatModel{inner: m, provider: provider, model: name, ref: ref}
}

type tracedChatModel struct {
	inner    model.BaseChatModel
	provider string
	model    string
	ref      string
}

func (m *tracedChatModel) traced() {}

func (m *tracedChatModel) start(ctx context.Context) (context.Context, trace.Span) {
	name := m.model
	if name == "" {
		name = "unknown"
	}
	ctx, span := observability.Tracer().Start(ctx, "chat "+name,
		trace.WithSpanKind(trace.SpanKindClient))
	attrs := []attribute.KeyValue{
		attribute.String(attrOperation, "chat"),
		attribute.String(attrModel, m.model),
	}
	if m.provider != "" {
		attrs = append(attrs, attribute.String(attrProvider, m.provider))
	}
	span.SetAttributes(attrs...)
	return ctx, span
}

func (m *tracedChatModel) Generate(ctx context.Context, input []*schema.Message, opts ...model.Option) (*schema.Message, error) {
	ctx, span := m.start(ctx)
	observability.SetMessages(span, "gen_ai.input.messages", stripSystem(input))
	observability.SetSystemInstructions(span, joinSystem(input))
	out, err := m.inner.Generate(ctx, input, opts...)
	if err != nil {
		finishSpan(span, err)
		return nil, err
	}
	m.recordOutput(span, out, 0, 0)
	span.SetStatus(codes.Ok, "")
	span.End()
	return out, nil
}

func (m *tracedChatModel) Stream(ctx context.Context, input []*schema.Message, opts ...model.Option) (*schema.StreamReader[*schema.Message], error) {
	reqStart := time.Now()
	ctx, span := m.start(ctx)
	observability.SetMessages(span, "gen_ai.input.messages", stripSystem(input))
	observability.SetSystemInstructions(span, joinSystem(input))
	stream, err := m.inner.Stream(ctx, input, opts...)
	if err != nil {
		finishSpan(span, err)
		return nil, err
	}
	copies := stream.Copy(2)
	go func() {
		defer span.End()
		defer copies[1].Close()
		var chunks []*schema.Message
		var firstAt time.Time
		var usage *schema.TokenUsage
		var finish string
		for {
			chunk, rerr := copies[1].Recv()
			if errors.Is(rerr, io.EOF) {
				break
			}
			if rerr != nil {
				finishSpan(span, rerr)
				return
			}
			if firstAt.IsZero() {
				firstAt = time.Now()
			}
			chunks = append(chunks, chunk)
			if chunk != nil && chunk.ResponseMeta != nil {
				if chunk.ResponseMeta.Usage != nil {
					usage = chunk.ResponseMeta.Usage
				}
				if chunk.ResponseMeta.FinishReason != "" {
					finish = chunk.ResponseMeta.FinishReason
				}
			}
		}
		merged, merr := schema.ConcatMessages(chunks)
		if merr != nil {
			finishSpan(span, merr)
			return
		}
		var ttft time.Duration
		if !firstAt.IsZero() {
			ttft = firstAt.Sub(reqStart)
			span.SetAttributes(attribute.Float64(attrTTFT, ttft.Seconds()))
		}
		m.recordOutputWithFinish(span, merged, usage, finish, time.Since(reqStart), ttft)
		span.SetStatus(codes.Ok, "")
	}()
	return copies[0], nil
}

func (m *tracedChatModel) recordOutput(span trace.Span, out *schema.Message, ttft, elapsed time.Duration) {
	m.recordOutputWithFinish(span, out, usageOf(out), finishOf(out), elapsed, ttft)
}

func (m *tracedChatModel) recordOutputWithFinish(span trace.Span, out *schema.Message, usage *schema.TokenUsage, finish string, elapsed, ttft time.Duration) {
	observability.SetMessages(span, "gen_ai.output.messages", []*schema.Message{out})
	if finish != "" {
		span.SetAttributes(attribute.StringSlice(attrFinish, []string{finish}))
	}
	in, outTok := usageTokens(usage)
	estimated := false
	if in == 0 && outTok == 0 && out != nil {
		outTok = estimateTokens(out.Content + out.ReasoningContent)
		estimated = outTok > 0
	}
	span.SetAttributes(
		attribute.Int(attrInputTok, in),
		attribute.Int(attrOutputTok, outTok),
		attribute.Int(attrTotalTok, in+outTok),
	)
	if estimated {
		span.SetAttributes(attribute.Bool(attrEstimated, true))
	}
	if ttft > 0 && outTok > 1 && elapsed > ttft {
		span.SetAttributes(attribute.Float64(attrTPOT, (elapsed-ttft).Seconds()/float64(outTok-1)))
	}
}

func usageOf(m *schema.Message) *schema.TokenUsage {
	if m == nil || m.ResponseMeta == nil {
		return nil
	}
	return m.ResponseMeta.Usage
}

func finishOf(m *schema.Message) string {
	if m == nil || m.ResponseMeta == nil {
		return ""
	}
	return m.ResponseMeta.FinishReason
}

func usageTokens(u *schema.TokenUsage) (int, int) {
	if u == nil {
		return 0, 0
	}
	return u.PromptTokens, u.CompletionTokens
}

// estimateTokens is a fallback for providers that omit usage. Roughly two runes
// per token for mixed Chinese/English text.
func estimateTokens(s string) int {
	n := utf8.RuneCountInString(strings.TrimSpace(s))
	if n == 0 {
		return 0
	}
	return (n + 1) / 2
}

func splitSystemMessages(msgs []*schema.Message) ([]string, []*schema.Message) {
	var sys []string
	rest := make([]*schema.Message, 0, len(msgs))
	for _, m := range msgs {
		if m != nil && m.Role == schema.System {
			if m.Content != "" {
				sys = append(sys, m.Content)
			}
			continue
		}
		rest = append(rest, m)
	}
	return sys, rest
}

func stripSystem(msgs []*schema.Message) []*schema.Message {
	_, rest := splitSystemMessages(msgs)
	return rest
}

func joinSystem(msgs []*schema.Message) string {
	sys, _ := splitSystemMessages(msgs)
	return strings.Join(sys, "\n\n")
}

// WrapChatModel exposes the tracing wrapper to the entry point (memory extractor).
func WrapChatModel(m model.BaseChatModel, ref string) model.BaseChatModel {
	return wrapChatModel(m, ref)
}

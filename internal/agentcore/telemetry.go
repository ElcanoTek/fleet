package agentcore

import (
	"context"

	"charm.land/fantasy"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/codes"
	"go.opentelemetry.io/otel/trace"

	"github.com/ElcanoTek/fleet/internal/telemetry"
)

// Spans of the governed loop (#2052, docs/TELEMETRY.md). Run opens
// agentcore.run; every provider call inside it is a model.* span (tracedModel)
// and every tool dispatch a tool.call span (panicContainedTool.Run). Sandbox,
// broker and DB spans nest under the tool span through the context fantasy
// hands the tool.
//
// Attributes are ids and sizes only: provider and model ids, message/tool
// counts, token usage, the tool's name and call id, input/output byte counts.
// Prompt text, completions, tool arguments and results never reach a span —
// see internal/telemetry.

// Span attribute keys for the loop. The gen_ai.* names follow the
// OpenTelemetry GenAI semantic conventions so a trace backend that knows them
// renders model spans natively.
const (
	attrGenAISystem       = attribute.Key("gen_ai.system")
	attrGenAIModel        = attribute.Key("gen_ai.request.model")
	attrGenAIInputTokens  = attribute.Key("gen_ai.usage.input_tokens")
	attrGenAIOutputTokens = attribute.Key("gen_ai.usage.output_tokens")
	attrGenAIFinish       = attribute.Key("gen_ai.response.finish_reasons")
	attrCacheReadTokens   = attribute.Key("fleet.usage.cache_read_tokens")
	attrMessages          = attribute.Key("fleet.model.messages")
	attrTools             = attribute.Key("fleet.model.tools")
	attrToolName          = attribute.Key("gen_ai.tool.name")
	attrToolCallID        = attribute.Key("gen_ai.tool.call.id")
	attrToolInputBytes    = attribute.Key("fleet.tool.input_bytes")
	attrToolOutputBytes   = attribute.Key("fleet.tool.output_bytes")
	attrToolIsError       = attribute.Key("fleet.tool.is_error")
)

// tracedModel wraps a fantasy.LanguageModel with one span per provider call.
// It is applied where the loop builds its fantasy agent (buildAgent), so the
// primary model and every fallback are traced alike; Provider()/Model() and
// any other method pass through the embedded model.
type tracedModel struct{ fantasy.LanguageModel }

// withModelTracing wraps m once (re-wrapping a tracedModel is a no-op).
func withModelTracing(m fantasy.LanguageModel) fantasy.LanguageModel {
	if m == nil {
		return nil
	}
	if _, ok := m.(tracedModel); ok {
		return m
	}
	return tracedModel{m}
}

func (m tracedModel) start(ctx context.Context, op string, msgs, tools int) (context.Context, trace.Span) {
	ctx, span := telemetry.Start(ctx, "model."+op, trace.WithSpanKind(trace.SpanKindClient))
	if span.IsRecording() {
		span.SetAttributes(
			attrGenAISystem.String(m.Provider()),
			attrGenAIModel.String(m.Model()),
			attrMessages.Int(msgs),
			attrTools.Int(tools),
		)
	}
	return ctx, span
}

func recordUsage(span trace.Span, u fantasy.Usage, finish fantasy.FinishReason) {
	if !span.IsRecording() {
		return
	}
	span.SetAttributes(
		attrGenAIInputTokens.Int64(u.InputTokens),
		attrGenAIOutputTokens.Int64(u.OutputTokens),
		attrCacheReadTokens.Int64(u.CacheReadTokens),
	)
	if finish != "" {
		span.SetAttributes(attrGenAIFinish.StringSlice([]string{string(finish)}))
	}
}

func (m tracedModel) Generate(ctx context.Context, call fantasy.Call) (*fantasy.Response, error) {
	ctx, span := m.start(ctx, "generate", len(call.Prompt), len(call.Tools))
	resp, err := m.LanguageModel.Generate(ctx, call)
	if resp != nil {
		recordUsage(span, resp.Usage, resp.FinishReason)
	}
	telemetry.End(span, err)
	return resp, err
}

// Stream's span covers the whole stream: it ends when the consumer stops
// iterating (fantasy always drains or abandons the sequence), recording the
// finish part's usage and an error part's class.
func (m tracedModel) Stream(ctx context.Context, call fantasy.Call) (fantasy.StreamResponse, error) {
	ctx, span := m.start(ctx, "stream", len(call.Prompt), len(call.Tools))
	seq, err := m.LanguageModel.Stream(ctx, call)
	if err != nil {
		telemetry.End(span, err)
		return nil, err
	}
	return func(yield func(fantasy.StreamPart) bool) {
		var streamErr error
		defer func() { telemetry.End(span, streamErr) }()
		for part := range seq {
			switch part.Type {
			case fantasy.StreamPartTypeFinish:
				recordUsage(span, part.Usage, part.FinishReason)
			case fantasy.StreamPartTypeError:
				streamErr = part.Error
			default:
				// Content parts are the model's words: never recorded.
			}
			if !yield(part) {
				return
			}
		}
	}, nil
}

func (m tracedModel) GenerateObject(ctx context.Context, call fantasy.ObjectCall) (*fantasy.ObjectResponse, error) {
	ctx, span := m.start(ctx, "generate_object", len(call.Prompt), 0)
	resp, err := m.LanguageModel.GenerateObject(ctx, call)
	if resp != nil {
		recordUsage(span, resp.Usage, resp.FinishReason)
	}
	telemetry.End(span, err)
	return resp, err
}

func (m tracedModel) StreamObject(ctx context.Context, call fantasy.ObjectCall) (fantasy.ObjectStreamResponse, error) {
	ctx, span := m.start(ctx, "stream_object", len(call.Prompt), 0)
	seq, err := m.LanguageModel.StreamObject(ctx, call)
	if err != nil {
		telemetry.End(span, err)
		return nil, err
	}
	return func(yield func(fantasy.ObjectStreamPart) bool) {
		var streamErr error
		defer func() { telemetry.End(span, streamErr) }()
		for part := range seq {
			if part.Error != nil {
				streamErr = part.Error
			}
			if part.FinishReason != "" {
				recordUsage(span, part.Usage, part.FinishReason)
			}
			if !yield(part) {
				return
			}
		}
	}, nil
}

// startToolSpan opens the tool.call span for one dispatch. The tool's name
// and call id are identifiers the model chose from the roster fleet offered;
// the arguments are counted, never copied.
func startToolSpan(ctx context.Context, name string, call fantasy.ToolCall) (context.Context, trace.Span) {
	ctx, span := telemetry.Start(ctx, "tool.call")
	if span.IsRecording() {
		span.SetAttributes(
			attrToolName.String(name),
			attrToolCallID.String(call.ID),
			attrToolInputBytes.Int(len(call.Input)),
		)
	}
	return ctx, span
}

// endToolSpan records the response's size and error bit and ends the span. A
// tool-level error (IsError) is a failed call from the trace's point of view
// even though the loop carries on; its class is "tool_error".
func endToolSpan(span trace.Span, resp fantasy.ToolResponse, err error) {
	if span.IsRecording() {
		span.SetAttributes(
			attrToolOutputBytes.Int(len(resp.Content)+len(resp.Data)),
			attrToolIsError.Bool(resp.IsError),
		)
		if err == nil && resp.IsError {
			span.SetAttributes(telemetry.AttrErrClass.String("tool_error"))
			span.SetStatus(codes.Error, "tool_error")
		}
	}
	telemetry.End(span, err)
}

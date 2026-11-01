package observability

import (
	"encoding/json"
	"strings"

	"github.com/cloudwego/eino/schema"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/trace"
)

const (
	// maxTextBytes caps one text part; maxAttrBytes caps a whole serialized attribute.
	maxTextBytes = 8 * 1024
	maxAttrBytes = 64 * 1024
	truncAttr    = "tako.content.truncated"
)

type contentPart struct {
	Type      string `json:"type"`
	Content   string `json:"content,omitempty"`
	ID        string `json:"id,omitempty"`
	Name      string `json:"name,omitempty"`
	Arguments any    `json:"arguments,omitempty"`
	Response  string `json:"response,omitempty"`
	URI       string `json:"uri,omitempty"`
	Modality  string `json:"modality,omitempty"`
}

type contentMessage struct {
	Role  string        `json:"role"`
	Parts []contentPart `json:"parts"`
}

// SetMessages writes a semconv-shaped messages attribute (JSON string on the span).
func SetMessages(span trace.Span, key string, msgs []*schema.Message) {
	if !CaptureContent() || span == nil {
		return
	}
	body, truncated := MessagesJSON(msgs)
	span.SetAttributes(attribute.String(key, body))
	if truncated {
		span.SetAttributes(attribute.Bool(truncAttr, true))
	}
}

// MessagesJSON serializes messages into the semconv parts shape. Truncated reports
// that a text part or the whole attribute hit its cap.
func MessagesJSON(msgs []*schema.Message) (string, bool) {
	out := make([]contentMessage, 0, len(msgs))
	truncated := false
	for _, m := range msgs {
		if m == nil {
			continue
		}
		cm := contentMessage{Role: string(m.Role)}
		if m.Role == schema.Tool {
			response := m.Content
			if len(response) > maxTextBytes {
				response = cutBytes(response, maxTextBytes)
				truncated = true
			}
			cm.Parts = append(cm.Parts, contentPart{Type: "tool_call_response", ID: m.ToolCallID, Response: response})
		} else {
			parts, t := messageParts(m)
			truncated = truncated || t
			cm.Parts = append(cm.Parts, parts...)
		}
		out = append(out, cm)
	}
	body, err := json.Marshal(out)
	if err != nil {
		return "[]", false
	}
	return capJSON(body, &truncated)
}

func messageParts(m *schema.Message) ([]contentPart, bool) {
	var parts []contentPart
	truncated := false
	addText := func(typ, text string) {
		if text == "" {
			return
		}
		if len(text) > maxTextBytes {
			text = cutBytes(text, maxTextBytes)
			truncated = true
		}
		parts = append(parts, contentPart{Type: typ, Content: text})
	}
	for _, p := range m.MultiContent {
		switch {
		case p.Text != "":
			addText("text", p.Text)
		case p.ImageURL != nil:
			parts = append(parts, contentPart{Type: "uri", URI: safeURI(p.ImageURL.URL), Modality: "image"})
		case p.AudioURL != nil:
			parts = append(parts, contentPart{Type: "uri", URI: safeURI(p.AudioURL.URL), Modality: "audio"})
		case p.VideoURL != nil:
			parts = append(parts, contentPart{Type: "uri", URI: safeURI(p.VideoURL.URL), Modality: "video"})
		case p.FileURL != nil:
			parts = append(parts, contentPart{Type: "uri", URI: safeURI(p.FileURL.URL), Modality: "file"})
		}
	}
	for _, p := range m.UserInputMultiContent {
		switch {
		case p.Text != "":
			addText("text", p.Text)
		case p.Image != nil:
			parts = append(parts, contentPart{Type: "uri", URI: partURI(p.Image.URL, p.Image.Base64Data), Modality: "image"})
		case p.Audio != nil:
			parts = append(parts, contentPart{Type: "uri", URI: partURI(p.Audio.URL, p.Audio.Base64Data), Modality: "audio"})
		case p.Video != nil:
			parts = append(parts, contentPart{Type: "uri", URI: partURI(p.Video.URL, p.Video.Base64Data), Modality: "video"})
		case p.File != nil:
			parts = append(parts, contentPart{Type: "uri", URI: partURI(p.File.URL, p.File.Base64Data), Modality: "file"})
		}
	}
	for _, p := range m.AssistantGenMultiContent {
		switch {
		case p.Text != "":
			addText("text", p.Text)
		case p.Reasoning != nil:
			addText("reasoning", p.Reasoning.Text)
		case p.Image != nil:
			parts = append(parts, contentPart{Type: "uri", URI: partURI(p.Image.URL, p.Image.Base64Data), Modality: "image"})
		case p.Audio != nil:
			parts = append(parts, contentPart{Type: "uri", URI: partURI(p.Audio.URL, p.Audio.Base64Data), Modality: "audio"})
		case p.Video != nil:
			parts = append(parts, contentPart{Type: "uri", URI: partURI(p.Video.URL, p.Video.Base64Data), Modality: "video"})
		}
	}
	if len(parts) == 0 && m.Content != "" {
		addText("text", m.Content)
	}
	if m.ReasoningContent != "" {
		addText("reasoning", m.ReasoningContent)
	}
	for _, tc := range m.ToolCalls {
		args := any(tc.Function.Arguments)
		var parsed any
		if err := json.Unmarshal([]byte(tc.Function.Arguments), &parsed); err == nil {
			args = parsed
		}
		parts = append(parts, contentPart{Type: "tool_call", ID: tc.ID, Name: tc.Function.Name, Arguments: args})
	}
	return parts, truncated
}

// SetSystemInstructions writes the system prompt as a JSON parts array.
func SetSystemInstructions(span trace.Span, text string) {
	if !CaptureContent() || span == nil || strings.TrimSpace(text) == "" {
		return
	}
	body, truncated := marshalParts([]contentPart{{Type: "text", Content: text}})
	span.SetAttributes(attribute.String("gen_ai.system_instructions", body))
	if truncated {
		span.SetAttributes(attribute.Bool(truncAttr, true))
	}
}

// SetTextJSON writes a value as JSON; arguments/results are passed as raw strings
// when they are already JSON so the reader keeps their original bytes.
func SetTextJSON(span trace.Span, key string, text string) {
	if !CaptureContent() || span == nil {
		return
	}
	body, truncated := capJSON([]byte(text), nil)
	span.SetAttributes(attribute.String(key, body))
	if truncated {
		span.SetAttributes(attribute.Bool(truncAttr, true))
	}
}

func marshalParts(parts []contentPart) (string, bool) {
	body, err := json.Marshal(parts)
	if err != nil {
		return "[]", false
	}
	return capJSON(body, nil)
}

// capJSON truncates a serialized attribute to maxAttrBytes by keeping the longest
// valid UTF-8 prefix. The caller marks tako.content.truncated when reported.
func capJSON(body []byte, already *bool) (string, bool) {
	truncated := already != nil && *already
	if len(body) <= maxAttrBytes {
		return string(body), truncated
	}
	return cutBytes(string(body), maxAttrBytes), true
}

// cutBytes cuts on a rune boundary so the trace stays valid UTF-8.
func cutBytes(s string, max int) string {
	if len(s) <= max {
		return s
	}
	cut := max
	for cut > 0 && !utf8Start(s[cut]) {
		cut--
	}
	return s[:cut]
}

func utf8Start(b byte) bool {
	return b&0xC0 != 0x80
}

// safeURI keeps tako:// references and non-data URLs, but never ships inline bytes.
func safeURI(uri string) string {
	if strings.HasPrefix(uri, "data:") {
		return "tako://media/inline"
	}
	return uri
}

func partURI(url, base64 *string) string {
	if url != nil && *url != "" {
		return safeURI(*url)
	}
	if base64 != nil && *base64 != "" {
		return "tako://media/inline"
	}
	return ""
}

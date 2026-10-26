package diag

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"slices"
	"strconv"
	"strings"
	"sync"
	"unicode"
	"unicode/utf8"
)

// textHandler keeps routine progress compact and gives failures a readable
// layout. Clones share a lock so complete reports cannot interleave with logs.
type textHandler struct {
	base   slog.Handler
	output io.Writer
	mu     *sync.Mutex
	attrs  []slog.Attr
	groups []string
}

func NewTextHandler(output io.Writer, options *slog.HandlerOptions) slog.Handler {
	return &textHandler{base: slog.NewTextHandler(output, options), output: output, mu: &sync.Mutex{}}
}

func (h *textHandler) Enabled(ctx context.Context, level slog.Level) bool {
	return h.base.Enabled(ctx, level)
}

func (h *textHandler) WithAttrs(attrs []slog.Attr) slog.Handler {
	clone := *h
	clone.base = h.base.WithAttrs(attrs)
	clone.attrs = slices.Clone(h.attrs)
	// Bind groups now so a later WithGroup cannot move existing context.
	for _, attr := range attrs {
		clone.attrs = append(clone.attrs, groupAttr(h.groups, attr))
	}
	return &clone
}

func (h *textHandler) WithGroup(name string) slog.Handler {
	if name == "" {
		return h
	}
	clone := *h
	clone.base = h.base.WithGroup(name)
	clone.groups = append(slices.Clone(h.groups), name)
	return &clone
}

func groupAttr(groups []string, attr slog.Attr) slog.Attr {
	for i := len(groups) - 1; i >= 0; i-- {
		attr = slog.Group(groups[i], attr)
	}
	return attr
}

func (h *textHandler) Handle(ctx context.Context, record slog.Record) error {
	h.mu.Lock()
	defer h.mu.Unlock()
	if record.Level < slog.LevelError {
		return h.base.Handle(ctx, record)
	}
	var output bytes.Buffer
	if !record.Time.IsZero() {
		output.WriteString(record.Time.Format("2006-01-02 15:04:05.000Z07:00"))
		output.WriteByte(' ')
	}
	fmt.Fprintf(&output, "%s: %s\n", record.Level, visible(record.Message))
	attrs := slices.Clone(h.attrs)
	record.Attrs(func(attr slog.Attr) bool { attrs = append(attrs, groupAttr(h.groups, attr)); return true })
	var causes []map[string]any
	var runtimeInfo any
	for _, attr := range attrs {
		value := attr.Value.Resolve()
		if attr.Key == "causes" {
			if chain, ok := value.Any().([]map[string]any); ok {
				causes = chain
				continue
			}
		}
		if attr.Key == "runtime" {
			runtimeInfo = value.Any()
			continue
		}
		writeValue(&output, attr.Key, slogValue(value), 2)
	}
	var stack []string
	for i, cause := range causes {
		op, _ := cause["operation"].(string)
		message, _ := cause["message"].(string)
		if i == 0 {
			output.WriteString("\n  Details:\n")
			if op != "" {
				writeValue(&output, "operation", op, 4)
			}
			if message != record.Message {
				writeValue(&output, "message", message, 4)
			}
		} else {
			output.WriteString("\n  Caused by")
			if op != "" {
				fmt.Fprintf(&output, " %s", visible(op))
			}
			output.WriteString(":\n")
			writeValue(&output, "message", message, 4)
			if code, ok := cause["code"]; ok {
				writeValue(&output, "code", code, 4)
			}
		}
		if fields, ok := cause["context"].(map[string]any); ok {
			writeFields(&output, fields, 4)
		}
		if frames, ok := cause["stack"].([]string); ok {
			stack = frames
		} else if location, ok := cause["location"].(string); ok {
			writeValue(&output, "location", location, 4)
		}
	}
	if len(stack) > 0 {
		output.WriteString("\n  Stack trace:\n")
		for _, frame := range stack {
			// Separate the function from its location so neither becomes part
			// of a quoted blob or a long line containing multiple frames.
			if function, location, ok := strings.Cut(frame, " ("); ok {
				fmt.Fprintf(&output, "    %s\n      %s\n", visible(function), visible(strings.TrimSuffix(location, ")")))
			} else {
				fmt.Fprintf(&output, "    %s\n", visible(frame))
			}
		}
	}
	if runtimeInfo != nil {
		output.WriteByte('\n')
		writeValue(&output, "Runtime", runtimeInfo, 2)
	}
	_, err := h.output.Write(output.Bytes())
	return err
}

func slogValue(value slog.Value) any {
	if value.Kind() == slog.KindGroup {
		fields := map[string]any{}
		for _, attr := range value.Group() {
			fields[attr.Key] = slogValue(attr.Value.Resolve())
		}
		return fields
	}
	return value.Any()
}

func writeFields(output *bytes.Buffer, fields map[string]any, indent int) {
	keys := make([]string, 0, len(fields))
	for key := range fields {
		keys = append(keys, key)
	}
	slices.Sort(keys)
	for _, key := range keys {
		writeValue(output, key, fields[key], indent)
	}
}

// Nested metadata and subprocess output retain indentation. Unknown structured
// values use indented JSON instead of Go's map/struct debug representations.
func writeValue(output *bytes.Buffer, key string, value any, indent int) {
	pad := strings.Repeat(" ", indent)
	switch value := value.(type) {
	case map[string]any:
		fmt.Fprintf(output, "%s%s:\n", pad, visible(key))
		writeFields(output, value, indent+2)
	case string:
		value = strings.TrimSuffix(value, "\n")
		if strings.Contains(value, "\n") {
			fmt.Fprintf(output, "%s%s:\n", pad, visible(key))
			for _, line := range strings.Split(value, "\n") {
				fmt.Fprintf(output, "%s  %s\n", pad, visible(line))
			}
		} else {
			fmt.Fprintf(output, "%s%s: %s\n", pad, visible(key), visible(value))
		}
	case []string:
		fmt.Fprintf(output, "%s%s:\n", pad, visible(key))
		for _, item := range value {
			fmt.Fprintf(output, "%s  - %s\n", pad, visible(item))
		}
	case nil:
		fmt.Fprintf(output, "%s%s: null\n", pad, visible(key))
	default:
		data, err := json.MarshalIndent(value, "", "  ")
		if err != nil {
			writeValue(output, key, fmt.Sprintf("<unavailable: %T>", value), indent)
			return
		}
		writeValue(output, key, string(data), indent)
	}
}

// Escape terminal control bytes while leaving readable Unicode intact. Source
// metadata must not be able to insert unindented lines or terminal commands.
func visible(text string) string {
	if utf8.ValidString(text) && strings.IndexFunc(text, unicode.IsControl) < 0 {
		return text
	}
	quoted := strconv.Quote(text)
	return quoted[1 : len(quoted)-1]
}

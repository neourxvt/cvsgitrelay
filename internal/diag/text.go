package diag

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"os"
	"slices"
	"strconv"
	"strings"
	"sync"
	"time"
	"unicode"
	"unicode/utf8"
)

// textHandler renders routine logs as concise text and failures as readable
// reports. Clones share a lock so complete reports cannot interleave.
type textHandler struct {
	base   slog.Handler
	output io.Writer
	mu     *sync.Mutex
	attrs  []slog.Attr
	groups []string
	color  bool
}

func NewTextHandler(output io.Writer, options *slog.HandlerOptions) slog.Handler {
	return &textHandler{
		base:   slog.NewTextHandler(output, options),
		output: output,
		mu:     &sync.Mutex{},
		color:  colorEnabled(output),
	}
}

func colorEnabled(output io.Writer) bool {
	// Respect terminal detection and the user's opt-out before adding ANSI codes.
	if os.Getenv("NO_COLOR") != "" || strings.EqualFold(os.Getenv("TERM"), "dumb") {
		return false
	}
	file, ok := output.(*os.File)
	if !ok {
		return false
	}
	info, err := file.Stat()
	return err == nil && info.Mode()&os.ModeCharDevice != 0
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
		var output bytes.Buffer
		writeHeader(&output, record, h.color)
		output.WriteString(humanMessage(record.Message))
		var attrs []slog.Attr
		// Keep bound operation context out of routine text lines; failure reports
		// and JSON logs retain it for diagnosis and collection.
		record.Attrs(func(attr slog.Attr) bool {
			attrs = append(attrs, groupAttr(h.groups, attr))
			return true
		})
		if len(attrs) > 0 {
			output.WriteString("  ")
			for i, attr := range attrs {
				if i > 0 {
					output.WriteString("; ")
				}
				fmt.Fprintf(&output, "%s: %s", humanKey(attr.Key), humanValue(attr.Value.Resolve()))
			}
		}
		output.WriteByte('\n')
		_, err := h.output.Write(output.Bytes())
		return err
	}
	var output bytes.Buffer
	writeHeader(&output, record, h.color)
	output.WriteString(visible(record.Message))
	output.WriteByte('\n')
	attrs := slices.Clone(h.attrs)
	record.Attrs(func(attr slog.Attr) bool { attrs = append(attrs, groupAttr(h.groups, attr)); return true })
	var causes []map[string]any
	for _, attr := range attrs {
		value := attr.Value.Resolve()
		if attr.Key == "causes" {
			if chain, ok := value.Any().([]map[string]any); ok {
				causes = chain
				continue
			}
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
	_, err := h.output.Write(output.Bytes())
	return err
}

func writeHeader(output *bytes.Buffer, record slog.Record, color bool) {
	if !record.Time.IsZero() {
		output.WriteString(record.Time.Format("2006-01-02 15:04:05"))
		output.WriteByte(' ')
	}
	label := fmt.Sprintf("%-5s", strings.ToUpper(record.Level.String()))
	if color {
		fmt.Fprintf(output, "\x1b[%sm%s\x1b[0m", levelColor(record.Level), label)
	} else {
		output.WriteString(label)
	}
	output.WriteByte(' ')
}

func levelColor(level slog.Level) string {
	switch {
	case level >= slog.LevelError:
		return "31;1"
	case level >= slog.LevelWarn:
		return "33;1"
	case level >= slog.LevelInfo:
		return "36"
	default:
		return "2"
	}
}

func humanMessage(message string) string {
	message = visible(message)
	if message == "" {
		return message
	}
	first, size := utf8.DecodeRuneInString(message)
	return string(unicode.ToUpper(first)) + message[size:]
}

func humanKey(key string) string { return strings.ReplaceAll(visible(key), "_", " ") }

func humanValue(value slog.Value) string {
	switch value.Kind() {
	case slog.KindString:
		return visible(value.String())
	case slog.KindBool:
		return strconv.FormatBool(value.Bool())
	case slog.KindInt64:
		return strconv.FormatInt(value.Int64(), 10)
	case slog.KindUint64:
		return strconv.FormatUint(value.Uint64(), 10)
	case slog.KindFloat64:
		return strconv.FormatFloat(value.Float64(), 'g', -1, 64)
	case slog.KindDuration:
		return value.Duration().String()
	case slog.KindTime:
		return value.Time().Format(time.RFC3339)
	case slog.KindGroup:
		var fields []string
		for _, attr := range value.Group() {
			fields = append(fields, humanKey(attr.Key)+": "+humanValue(attr.Value.Resolve()))
		}
		return strings.Join(fields, ", ")
	default:
		return humanAny(value.Any())
	}
}

func humanAny(value any) string {
	switch value := value.(type) {
	case nil:
		return "none"
	case string:
		return visible(value)
	case []string:
		if len(value) == 0 {
			return "none"
		}
		parts := make([]string, len(value))
		for i := range value {
			parts[i] = visible(value[i])
		}
		return strings.Join(parts, ", ")
	case error:
		return visible(value.Error())
	default:
		return visible(fmt.Sprint(value))
	}
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

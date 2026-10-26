// Package diag attaches structured context and stack frames without logging
// errors at every layer. The CLI emits the complete chain once per failed run.
package diag

import (
	"errors"
	"fmt"
	"log/slog"
	"runtime"
	"strings"
)

type Error struct {
	Code    string
	Op      string
	Message string
	Fields  map[string]any
	Cause   error
	Stack   []uintptr
}

func (e *Error) Error() string {
	s := e.Op + ": " + e.Message
	if e.Cause != nil {
		s += ": " + e.Cause.Error()
	}
	return s
}

func (e *Error) Unwrap() error { return e.Cause }

func New(code, op, message string, cause error, fields ...any) error {
	e := &Error{Code: code, Op: op, Message: message, Cause: cause, Fields: map[string]any{}}
	for i := 0; i+1 < len(fields); i += 2 {
		e.Fields[fmt.Sprint(fields[i])] = fields[i+1]
	}
	e.Stack = make([]uintptr, 32)
	e.Stack = e.Stack[:runtime.Callers(2, e.Stack)]
	return e
}

func Code(err error) string {
	var e *Error
	if errors.As(err, &e) {
		return e.Code
	}
	return "internal"
}

// Details retains structured causes for JSON collection and the readable text
// renderer. The text layout does not alter the machine-readable error schema.
func Details(err error, stacks bool) []map[string]any {
	var chain []map[string]any
	var deepestStack []string
	deepestIndex := -1
	for err != nil {
		d := map[string]any{}
		if e, ok := err.(*Error); ok {
			d["code"], d["operation"], d["message"], d["context"] = e.Code, e.Op, e.Message, e.Fields
			if stacks && len(e.Stack) > 0 {
				var frames []string
				it := runtime.CallersFrames(e.Stack)
				for {
					f, more := it.Next()
					frames = append(frames, fmt.Sprintf("%s (%s:%d)", f.Function, f.File, f.Line))
					if !more {
						break
					}
				}
				d["location"] = frames[0]
				deepestStack, deepestIndex = frames, len(chain)
			}
		} else {
			d["message"] = err.Error()
		}
		chain = append(chain, d)
		err = errors.Unwrap(err)
	}
	// One full stack at the originating failure plus boundary locations avoids
	// repeating common caller frames for every wrapping layer.
	if deepestIndex >= 0 {
		chain[deepestIndex]["stack"] = deepestStack
	}
	return chain
}

func Failure(log *slog.Logger, err error, stacks bool) {
	message := "operation failed"
	if e, ok := err.(*Error); ok {
		message = e.Message
	}
	log.Error(message, "code", Code(err), "causes", Details(err, stacks))
}

// Redact removes the entire locator as well as a password embedded in a CVS
// authority. Raw argv/environment and revision contents are never diagnostic data.
func Redact(s, root string) string {
	if root == "" {
		return s
	}
	s = strings.ReplaceAll(s, root, "[cvsroot]")
	if at := strings.LastIndexByte(root, '@'); at >= 0 {
		prefix := root[:at]
		if colon := strings.LastIndexByte(prefix, ':'); colon >= 0 {
			secret := prefix[colon+1:]
			if secret != "" {
				s = strings.ReplaceAll(s, secret, "[redacted]")
			}
		}
	}
	return s
}

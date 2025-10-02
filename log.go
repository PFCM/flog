// package flog is a thin layer over the standard log/slog package that stores a
// logger inside a context, rather than using the default logger in the package
// variable or having to manually pass one around.
//
// TODO:
//   - figure out a nice way to vary levels for sub-loggers
//   - groups
package flog

import (
	"context"
	"fmt"
	"log/slog"
	"os"
	"runtime"
	"strings"
	"sync"
	"time"

	"golang.org/x/net/trace"
)

// contextKeyType is the type of the context key used to store the logger. This
// makes it extra unlikely that there could be any conflicts.
type contextKeyType = string

// contextKey is the key under which a logger should be available in the
// context.
const contextKey = contextKeyType("logger")

// FromContext fetches the current logger from the context, or nil if there
// isn't one.
func FromContext(ctx context.Context) *slog.Logger {
	if v := ctx.Value(contextKey); v != nil {
		return v.(*slog.Logger)
	}
	return nil
}

// WithLogger returns a child context containing the provided logger that can be
// used with the rest of this package's functions.
func WithLogger(ctx context.Context, l *slog.Logger) context.Context {
	return context.WithValue(ctx, contextKey, l)
}

// With returns a new child context whose associated logger is the result of
// calling the With method of the logger in the parent context with the given
// arguments. If there is no logger in the parent context, slog.Default() will
// be used.
func With(ctx context.Context, args ...any) context.Context {
	l := FromContext(ctx)
	if l == nil {
		l = slog.Default()
	}
	return WithLogger(ctx, l.With(args...))
}

// WithGroup returns a new child context whose associated logger is the result
// of calling the WithGroup method of the logger in the parent context with the
// given arguments. If there is no logger in the parent context, slog.Default()
// will be used.
func WithGroup(ctx context.Context, name string) context.Context {
	l := FromContext(ctx)
	if l == nil {
		l = slog.Default()
	}
	return WithLogger(ctx, l.WithGroup(name))
}

// MultiHandler is an slog.Handler that wraps a number of other Handlers,
// emitting log records to each of them.
type MultiHandler struct {
	handlers []slog.Handler
}

// NewMultiHandler returns an slog.Handler that handles log records by passing
// them to each of the provided handlers in turn.
func NewMultiHandler(handlers ...slog.Handler) *MultiHandler {
	return &MultiHandler{handlers: handlers}
}

func (mh *MultiHandler) Enabled(ctx context.Context, level slog.Level) bool {
	for _, h := range mh.handlers {
		if h.Enabled(ctx, level) {
			return true
		}
	}
	return false
}

func (mh *MultiHandler) Handle(ctx context.Context, r slog.Record) error {
	for _, h := range mh.handlers {
		if err := h.Handle(ctx, r); err != nil {
			return err
		}
	}
	return nil
}

func (mh *MultiHandler) WithAttrs(attrs []slog.Attr) slog.Handler {
	handlers := make([]slog.Handler, len(mh.handlers))
	for i, h := range mh.handlers {
		// According to the documentation: "the Handler owns the slice,
		// it may retain, modify or discard it". Therefore we need to
		// make a copy for each Handler, in case they do modify it.
		attrsCopy := make([]slog.Attr, len(attrs))
		copy(attrsCopy, attrs)
		handlers[i] = h.WithAttrs(attrsCopy)
	}
	return &MultiHandler{handlers: handlers}
}

func (mh *MultiHandler) WithGroup(name string) slog.Handler {
	handlers := make([]slog.Handler, len(mh.handlers))
	for i, h := range handlers {
		handlers[i] = h.WithGroup(name)
	}
	return &MultiHandler{handlers: handlers}
}

// TraceHandler is a slog.Handler that emits records to a net/http/trace.Trace,
// if there is one in the context. The format is similar to the default log handler,
// although the time is always ignored.
type TraceHandler struct {
	addSource   bool
	level       slog.Leveler
	replaceAttr func(groups []string, a slog.Attr) slog.Attr

	groups []string
	attrs  []slog.Attr
}

func NewTraceHandler(opts *slog.HandlerOptions) *TraceHandler {
	if opts == nil {
		opts = &slog.HandlerOptions{}
	}
	if opts.Level == nil {
		opts.Level = slog.LevelInfo
	}
	return &TraceHandler{
		addSource:   opts.AddSource,
		level:       opts.Level,
		replaceAttr: opts.ReplaceAttr,
	}
}

func (t *TraceHandler) Enabled(ctx context.Context, level slog.Level) bool {
	// No levels are enabled if there's no trace in the context.
	if _, ok := trace.FromContext(ctx); !ok {
		return false
	}
	return level >= t.level.Level()
}

func (t *TraceHandler) Handle(ctx context.Context, r slog.Record) error {
	// TODO: use replaceAttr?
	tr, ok := trace.FromContext(ctx)
	if !ok {
		return nil
	}
	if r.Level < t.level.Level() {
		return nil
	}
	buf := bufferPool.Get().(*strings.Builder)
	defer func() {
		buf.Reset()
		bufferPool.Put(buf)
	}()
	fmt.Fprintf(buf, "%s %s ", r.Level, r.Message)

	writeAttr := func(a slog.Attr) {
		fmt.Fprintf(buf, "%s=%s ", a.Key, a.Value)
	}
	if t.addSource && r.PC != 0 {
		writeAttr(source(r))
	}
	for _, a := range t.attrs {
		writeAttr(a)
	}
	r.Attrs(func(a slog.Attr) bool {
		writeAttr(a)
		return true
	})
	tr.LazyPrintf(buf.String())
	return nil
}

func source(r slog.Record) slog.Attr {
	frames := runtime.CallersFrames([]uintptr{r.PC})
	f, _ := frames.Next()
	return slog.Group(slog.SourceKey,
		"file", fmt.Sprintf("%s:%d", f.File, f.Line),
		"func", f.Function,
	)
}

var bufferPool = sync.Pool{
	New: func() any {
		return &strings.Builder{}
	},
}

func (t *TraceHandler) WithAttrs(attrs []slog.Attr) slog.Handler {
	for i, a := range attrs {
		a.Key = strings.Join(append(t.groups, a.Key), ".")
		attrs[i] = a
	}
	return &TraceHandler{
		addSource:   t.addSource,
		level:       t.level,
		replaceAttr: t.replaceAttr,

		groups: t.groups,
		attrs:  append(t.attrs, attrs...),
	}
}

func (t *TraceHandler) WithGroup(name string) slog.Handler {
	return &TraceHandler{
		addSource:   t.addSource,
		level:       t.level,
		replaceAttr: t.replaceAttr,

		groups: append(t.groups, name),
		attrs:  t.attrs,
	}
}

////////////////////////////////////////////////////////////////////////////////
////////////////////////////////////////////////////////////////////////////////
// These are the actual logging functions.
////////////////////////////////////////////////////////////////////////////////
////////////////////////////////////////////////////////////////////////////////

func log(ctx context.Context, level slog.Level, msg string) {
	l := FromContext(ctx)
	if l == nil {
		panic("no logger in provided context")
	}
	if !l.Enabled(ctx, level) {
		return
	}
	var pcs [1]uintptr
	runtime.Callers(3, pcs[:]) // skip Callers, this function and the caller.
	r := slog.NewRecord(time.Now(), level, msg, pcs[0])
	if err := l.Handler().Handle(ctx, r); err != nil {
		// TODO: ?
		Errorf(ctx, "received error handling log: %v", err)
	}
}

// Debug logs a message at debug level using the logger in the context. It will
// panic if there is no logger in the context.
func Debug(ctx context.Context, msg string, args ...any) {
	log(With(ctx, args...), slog.LevelDebug, msg)
}

// Debugf is like Debug, but formats the message and args using fmt.Sprintf
// before logging. It is therefore not possible to provide log attributes in the
// same call without using With.
func Debugf(ctx context.Context, msg string, args ...any) {
	log(ctx, slog.LevelDebug, fmt.Sprintf(msg, args...))
}

// Info logs a message at info level using the logger in the context. It will
// panic if there is no logger in the context.
func Info(ctx context.Context, msg string, args ...any) {
	log(With(ctx, args...), slog.LevelInfo, msg)
}

// Infof is like Info, but formats the message and args using fmt.Sprintf
// before logging. It is therefore not possible to provide log attributes in the
// same call without using With.
func Infof(ctx context.Context, msg string, args ...any) {
	log(ctx, slog.LevelInfo, fmt.Sprintf(msg, args...))
}

// Warn logs a message at warning level using the logger in the context. It will
// panic if there is no logger in the context.
func Warn(ctx context.Context, msg string, args ...any) {
	log(With(ctx, args...), slog.LevelWarn, msg)
}

// Warnf is like Warn, but formats the message and args using fmt.Sprintf
// before logging. It is therefore not possible to provide log attributes in the
// same call without using With.
func Warnf(ctx context.Context, msg string, args ...any) {
	log(ctx, slog.LevelWarn, fmt.Sprintf(msg, args...))
}

// Error logs a message at error level using the logger in the context. It will
// panic if there is no logger in the context.
func Error(ctx context.Context, msg string, args ...any) {
	log(ctx, slog.LevelError, msg)
}

// Errorf is like Error, but formats the message and args using fmt.Sprintf
// before logging. It is therefore not possible to provide log attributes in the
// same call without using With. As a special case the first error provided in
// args will be added as an "error" attribute.
func Errorf(ctx context.Context, msg string, args ...any) {
	for _, a := range args {
		if err, ok := a.(error); ok {
			ctx = With(ctx, "error", err)
		}
	}
	log(ctx, slog.LevelError, fmt.Sprintf(msg, args...))
}

// Fatal logs the provided message with Error and a "fatal=true" attribute, and
// then calls os.Exit(1). Typically only makes sense inside a main function.
func Fatal(ctx context.Context, msg string, args ...any) {
	log(With(ctx, append(args, "fatal", true)...), slog.LevelError, msg)
	os.Exit(1)
}

// Fatalf is like Fatal, but formats the message and args with fmt.Sprintf.
func Fatalf(ctx context.Context, msg string, args ...any) {
	log(With(ctx, "fatal", true), slog.LevelError, fmt.Sprintf(msg, args...))
	os.Exit(1)
}

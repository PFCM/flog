package flog

import (
	"context"
	"fmt"
	"log/slog"
	"strings"
	"testing"
)

// TestContext returns a context derived from context.Background, with a logger that will forward every logging
// call to t.Log regardless of level.
func TestContext(t *testing.T) context.Context {
	return WithLogger(context.Background(), slog.New(&testHandler{t: t}))
}

type testHandler struct {
	t *testing.T

	groups []string
	attrs  []slog.Attr
}

func (th *testHandler) Enabled(context.Context, slog.Level) bool { return true }

func (th *testHandler) Handle(_ context.Context, r slog.Record) error {
	buf := bufferPool.Get().(*strings.Builder)
	defer func() {
		buf.Reset()
		bufferPool.Put(buf)
	}()
	fmt.Fprintf(buf, "%s %s ", r.Level, r.Message)

	writeAttr := func(a slog.Attr) {
		fmt.Fprintf(buf, "%s=%s ", a.Key, a.Value)
	}
	// Always include source lines in tests.
	writeAttr(source(r))
	for _, a := range th.attrs {
		writeAttr(a)
	}
	r.Attrs(func(a slog.Attr) bool {
		writeAttr(a)
		return true
	})
	th.t.Log(buf.String())
	return nil
}

func (th *testHandler) WithAttrs(attrs []slog.Attr) slog.Handler {
	for i, a := range attrs {
		a.Key = strings.Join(append(th.groups, a.Key), ".")
		attrs[i] = a
	}
	return &testHandler{
		t:      th.t,
		groups: th.groups,
		attrs:  append(th.attrs, attrs...),
	}
}

func (th *testHandler) WithGroup(name string) slog.Handler {
	return &testHandler{
		t:      th.t,
		groups: append(th.groups, name),
		attrs:  th.attrs,
	}
}

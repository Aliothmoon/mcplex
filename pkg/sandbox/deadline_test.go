package sandbox

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"
)

func TestExtendableDeadlineExtendsImplicitDefaultAndCapsAtMax(t *testing.T) {
	dlCtx, cancel := newExtendableDeadlineCtx(context.Background(), 120*time.Millisecond, 600*time.Millisecond, false)
	defer cancel()

	// Extend past the initial 120ms default; should stay alive past 200ms.
	if !dlCtx.extendOrNote("sh.run", 300*time.Millisecond) {
		t.Fatal("expected implicit default deadline to extend")
	}
	select {
	case <-dlCtx.Done():
		t.Fatalf("context expired before extended deadline: %v", dlCtx.Err())
	case <-time.After(220 * time.Millisecond):
	}

	// Request beyond maxDur (600ms) -> capped at maxDur and still expires cleanly with DeadlineExceeded.
	dlCtx.extendOrNote("sh.run", 5*time.Second)
	select {
	case <-dlCtx.Done():
		if !errors.Is(dlCtx.Err(), context.DeadlineExceeded) {
			t.Fatalf("expected DeadlineExceeded, got %v", dlCtx.Err())
		}
	case <-time.After(900 * time.Millisecond):
		t.Fatal("expected deadline to fire at maxDur cap")
	}
}

func TestExtendableDeadlineRespectsExplicitTimeoutAndNotesClip(t *testing.T) {
	dlCtx, cancel := newExtendableDeadlineCtx(context.Background(), 120*time.Millisecond, 2*time.Second, true)
	defer cancel()

	if dlCtx.extendOrNote("sh.exec", 5*time.Second) {
		t.Fatal("explicit outer timeout must NOT be extended")
	}
	if note := dlCtx.clippedTimeoutNote(); !strings.Contains(note, "sh.exec requested timeoutMs=5000") {
		t.Fatalf("expected clipped note, got %q", note)
	}
	select {
	case <-dlCtx.Done():
		if !errors.Is(dlCtx.Err(), context.DeadlineExceeded) {
			t.Fatalf("expected DeadlineExceeded, got %v", dlCtx.Err())
		}
	case <-time.After(500 * time.Millisecond):
		t.Fatal("expected explicit timeout to fire on schedule")
	}
}

type dummyFsRmBinder struct{}

func (dummyFsRmBinder) Bind(env *ExecutionEnv) error {
	return env.SetNamespace("fs", map[string]AsyncHostFunc{
		"rm": func(_ context.Context, _ []any) (any, error) {
			return nil, nil
		},
	}, nil)
}

func TestOpBudgetFsRmHint(t *testing.T) {
	_, err := Execute(context.Background(), ExecRequest{
		Code:   `for (let i = 0; i < 6; i++) await fs.rm("x");`,
		MaxOps: 3,
	}, dummyFsRmBinder{})
	if err == nil {
		t.Fatal("expected op budget error")
	}
	if !strings.Contains(err.Error(), "for recursive directory deletion, use built-in run_command instead of looping fs.rm") {
		t.Fatalf("expected fs.rm escalation hint in op budget error, got: %v", err)
	}
}

package sandbox

import (
	"context"
	"testing"
	"time"
)

type waveTestBinder struct{}

func (waveTestBinder) Bind(env *ExecutionEnv) error {
	return env.SetNamespace("demo", map[string]AsyncHostFunc{
		"slow": func(ctx context.Context, args []any) (any, error) {
			ms := 25
			if len(args) > 0 {
				if n, ok := args[0].(int64); ok {
					ms = int(n)
				} else if f, ok := args[0].(float64); ok {
					ms = int(f)
				}
			}
			select {
			case <-time.After(time.Duration(ms) * time.Millisecond):
				return ms, nil
			case <-ctx.Done():
				return nil, ctx.Err()
			}
		},
	}, nil)
}

func TestOpTraceWaveGroupingForSequentialAndPromiseAll(t *testing.T) {
	res, err := Execute(context.Background(), ExecRequest{
		Cwd: t.TempDir(),
		Code: `
			await demo.slow(15);
			await Promise.all([demo.slow(40), demo.slow(10), demo.slow(25)]);
			await demo.slow(15);
		`,
	}, waveTestBinder{})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(res.Traces) != 5 {
		t.Fatalf("expected 5 traces, got %d: %+v", len(res.Traces), res.Traces)
	}
	bySeq := map[int]OpTrace{}
	for _, tr := range res.Traces {
		bySeq[tr.Seq] = tr
	}
	// Seq 1 = Wave 1 (sequential), Seq 2..4 = Wave 2 (Promise.all parallel ×3), Seq 5 = Wave 3 (sequential)
	if bySeq[1].Wave != 1 {
		t.Fatalf("expected seq 1 in wave 1, got %+v", bySeq[1])
	}
	if bySeq[2].Wave != 2 || bySeq[3].Wave != 2 || bySeq[4].Wave != 2 {
		t.Fatalf("expected seq 2..4 in same wave 2, got w2=%d w3=%d w4=%d", bySeq[2].Wave, bySeq[3].Wave, bySeq[4].Wave)
	}
	if bySeq[5].Wave != 3 {
		t.Fatalf("expected seq 5 in wave 3, got %+v", bySeq[5])
	}
	if bySeq[5].StartMs < bySeq[2].StartMs+bySeq[2].DurationMs-5 {
		t.Fatalf("expected seq 5 StartMs (%d) after parallel wave 2 completed (%d+%d)", bySeq[5].StartMs, bySeq[2].StartMs, bySeq[2].DurationMs)
	}
}

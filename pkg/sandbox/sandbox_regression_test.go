package sandbox

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"strings"
	"sync"
	"testing"
	"time"
	"unicode/utf8"

	"github.com/aliothmoon/mcplex/pkg/output"
	"github.com/dop251/goja"
)

type testDynamicObj struct {
	data map[string]string
	vm   *goja.Runtime
}

func (d *testDynamicObj) Get(key string) goja.Value {
	if v, ok := d.data[key]; ok {
		return d.vm.ToValue(v)
	}
	return nil
}
func (d *testDynamicObj) Set(string, goja.Value) bool { return false }
func (d *testDynamicObj) Has(key string) bool         { _, ok := d.data[key]; return ok }
func (d *testDynamicObj) Delete(string) bool          { return false }
func (d *testDynamicObj) Keys() []string {
	keys := make([]string, 0, len(d.data))
	for k := range d.data {
		keys = append(keys, k)
	}
	return keys
}

type regressionBinder struct {
	mu          sync.Mutex
	capturedBuf []byte
}

func (b *regressionBinder) Bind(env *ExecutionEnv) error {
	dyn := env.VM.NewDynamicObject(&testDynamicObj{
		data: map[string]string{"alpha": "one", "beta": "two"},
		vm:   env.VM,
	})
	if err := env.VM.Set("dynGlobal", dyn); err != nil {
		return err
	}
	return env.SetNamespace("testcap", map[string]AsyncHostFunc{
		"panicAsync": func(ctx context.Context, args []any) (any, error) {
			panic("simulated host goroutine panic")
		},
		"inspectBytesAsync": func(ctx context.Context, args []any) (any, error) {
			// Wait briefly so guest JS has already mutated its Uint8Array on the VM thread before we read args[0].
			time.Sleep(15 * time.Millisecond)
			if len(args) > 0 {
				if raw, ok := args[0].([]byte); ok {
					b.mu.Lock()
					b.capturedBuf = bytes.Clone(raw)
					b.mu.Unlock()
					return append([]byte(nil), raw...), nil
				}
			}
			return nil, errors.New("expected []byte argument")
		},
	}, nil)
}

func TestSafeExportCyclesDAGMaxDepthNaNAndHugeSparseArray(t *testing.T) {
	// 1. Cyclic object + shared DAG diamond + NaN / ±Infinity
	res, err := Execute(context.Background(), ExecRequest{
		Cwd: t.TempDir(),
		Code: `
			const shared = { ok: 7 };
			const cyc: any = { left: shared, right: shared, nan: NaN, inf: Infinity, ninf: -Infinity };
			cyc.self = cyc;
			print(NaN, Infinity, -Infinity, cyc);
			return cyc;
		`,
	})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	rawJSON := output.MarshalResultForBudget(res.Value, 9600)
	var parsed map[string]any
	if err := json.Unmarshal([]byte(rawJSON), &parsed); err != nil {
		t.Fatalf("exported cyclic/NaN value was not valid JSON: %v\njson=%s", err, rawJSON)
	}
	if parsed["self"] != "[Circular]" {
		t.Fatalf("expected self=[Circular], got %v", parsed["self"])
	}
	left, _ := parsed["left"].(map[string]any)
	right, _ := parsed["right"].(map[string]any)
	if left["ok"] != float64(7) || right["ok"] != float64(7) {
		t.Fatalf("shared DAG references must export normally, got left=%v right=%v", left, right)
	}
	if parsed["nan"] != nil || parsed["inf"] != nil || parsed["ninf"] != nil {
		t.Fatalf("expected NaN/±Infinity to export as nil/null, got %v", parsed)
	}
	if len(res.Logs) != 1 || !strings.HasPrefix(res.Logs[0], "NaN Infinity -Infinity ") || !strings.Contains(res.Logs[0], `"[Circular]"`) {
		t.Fatalf("unexpected log output: %v", res.Logs)
	}

	// 2. Depth cap: values below maxExportDepth round-trip intact (no silent data truncation), deeper values
	// fail loudly for results, and console.log keeps a "[MaxDepth]" marker.
	resDepth, err := Execute(context.Background(), ExecRequest{
		Cwd: t.TempDir(),
		Code: `
			let cur: any = { leaf: true };
			for (let i = 0; i < 80; i++) {
				cur = { child: cur };
			}
			return cur;
		`,
	})
	if err != nil {
		t.Fatalf("unexpected depth error: %v", err)
	}
	depthJSON := output.MarshalResultForBudget(resDepth.Value, 9600)
	if strings.Contains(depthJSON, "[MaxDepth]") || !strings.Contains(depthJSON, `"leaf":true`) {
		t.Fatalf("80-level object must export intact, got: %.300s", depthJSON)
	}
	resLogDepth, err := Execute(context.Background(), ExecRequest{
		Cwd: t.TempDir(),
		Code: `
			let cur: any = { leaf: true };
			for (let i = 0; i < 1100; i++) {
				cur = { child: cur };
			}
			console.log(cur);
			return 1;
		`,
	})
	if err != nil {
		t.Fatalf("console.log of a too-deep object must not fail the exec: %v", err)
	}
	if len(resLogDepth.Logs) != 1 || !strings.Contains(resLogDepth.Logs[0], "[MaxDepth]") {
		t.Fatalf("expected [MaxDepth] marker in logged deep object, got %d logs", len(resLogDepth.Logs))
	}

	// 3. Sparse / huge Array OOM guard (new Array(1e9) must fail fast without allocating 16GB!)
	start := time.Now()
	_, errHuge := Execute(context.Background(), ExecRequest{
		Cwd:  t.TempDir(),
		Code: `return new Array(1_000_000_000);`,
	})
	if errHuge == nil {
		t.Fatal("expected RangeError on new Array(1e9), got nil")
	}
	var ee *ExecutionError
	if !errors.As(errHuge, &ee) || ee.Phase != "runtime" || !strings.Contains(ee.Message, "RangeError") {
		t.Fatalf("expected runtime RangeError, got: %+v", errHuge)
	}
	if time.Since(start) > 2*time.Second {
		t.Fatalf("huge sparse array check took too long: %s", time.Since(start))
	}
}

func TestSafeExportThrowingGettersHostileRejectionsAndCallStackLimit(t *testing.T) {
	// 1. Throwing getter on returned object must NOT panic Execute
	_, err := Execute(context.Background(), ExecRequest{
		Cwd:  t.TempDir(),
		Code: `return { get x() { throw new Error("getter boom"); } };`,
	})
	var ee *ExecutionError
	if !errors.As(err, &ee) || ee.Phase != "runtime" || !strings.Contains(ee.Message, "getter boom") {
		t.Fatalf("expected controlled runtime error for throwing getter, got: %+v", err)
	}

	// 2. Hostile thrown object with throwing message/stack/toString getters
	_, errRej := Execute(context.Background(), ExecRequest{
		Cwd:  t.TempDir(),
		Code: `throw { get message() { throw 1; }, get stack() { throw 2; }, toString() { throw 3; } };`,
	})
	if !errors.As(errRej, &ee) || ee.Phase != "runtime" {
		t.Fatalf("expected controlled runtime error for hostile rejection, got: %+v", errRej)
	}

	// 3. Infinite JS recursion hits SetMaxCallStackSize(MaxCallStackDepth) cleanly
	_, errStack := Execute(context.Background(), ExecRequest{
		Cwd:  t.TempDir(),
		Code: `function inf(): any { return inf(); } return inf();`,
	})
	if !errors.As(errStack, &ee) || ee.Phase != "runtime" {
		t.Fatalf("expected runtime error on call stack overflow, got: %+v", errStack)
	}

	// 4. Async host goroutine panic recovered cleanly
	_, errPanic := Execute(context.Background(), ExecRequest{
		Cwd:  t.TempDir(),
		Code: `return await (globalThis as any).testcap.panicAsync();`,
	}, &regressionBinder{})
	if !errors.As(errPanic, &ee) || ee.Phase != "runtime" || !strings.Contains(ee.Message, "panicAsync") {
		t.Fatalf("expected controlled runtime error from panicked async host op, got: %+v", errPanic)
	}
}

func TestProxyAndDynamicObjectExportForProcessEnvAndPayloads(t *testing.T) {
	t.Setenv("MCPLEX_PUBLIC_TEST_VAR", "visible_value")
	t.Setenv("MCPLEX_SECRET_BEARER", "top_secret_value")

	res, err := Execute(context.Background(), ExecRequest{
		Cwd:      t.TempDir(),
		Payloads: map[string]string{"k1": "hello", "k2": "world"},
		Code: `
			print(P);
			print(process.env);
			return {
				p: P,
				payloadsAlias: payloads,
				env: process.env,
				dyn: (globalThis as any).dynGlobal,
			};
		`,
	}, &regressionBinder{})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	root, ok := res.Value.(map[string]any)
	if !ok {
		t.Fatalf("expected root map, got %T", res.Value)
	}
	pMap, _ := root["p"].(map[string]any)
	if pMap["k1"] != "hello" || pMap["k2"] != "world" {
		t.Fatalf("P exported empty or wrong contents: %v", pMap)
	}
	envMap, _ := root["env"].(map[string]any)
	if envMap["MCPLEX_PUBLIC_TEST_VAR"] != "visible_value" {
		t.Fatalf("process.env did not export public env var: %v", envMap["MCPLEX_PUBLIC_TEST_VAR"])
	}
	if envMap["MCPLEX_SECRET_BEARER"] != redacted {
		t.Fatalf("process.env leaked secret bearer var: %v", envMap["MCPLEX_SECRET_BEARER"])
	}
	dynMap, _ := root["dyn"].(map[string]any)
	if dynMap["alpha"] != "one" || dynMap["beta"] != "two" {
		t.Fatalf("DynamicObject did not export contents: %v", dynMap)
	}
	if len(res.Logs) != 2 || !strings.Contains(res.Logs[0], `"k1":"hello"`) || !strings.Contains(res.Logs[1], `"MCPLEX_PUBLIC_TEST_VAR":"visible_value"`) {
		t.Fatalf("print(P) / print(process.env) did not log contents: %v", res.Logs)
	}
}

func TestConsoleLogReentrancyDoesNotDeadlock(t *testing.T) {
	res, err := Execute(context.Background(), ExecRequest{
		Cwd:     t.TempDir(),
		Timeout: 2 * time.Second,
		Code: `
			const nested = {
				get value() {
					console.log("inside getter");
					return 42;
				}
			};
			console.log(nested);

			const infiniteRec: any = {
				get selfLog() {
					console.log(infiniteRec);
					return "ok";
				}
			};
			console.log(infiniteRec);
			return "done";
		`,
	})
	if err != nil {
		t.Fatalf("unexpected error on re-entrant console.log: %v", err)
	}
	if len(res.Logs) < 2 || res.Logs[0] != "inside getter" || res.Logs[1] != `{"value":42}` {
		t.Fatalf("unexpected logs from re-entrant getter: %v", res.Logs)
	}
}

func TestSetTimeoutErrorPhaseAndAsyncInfiniteLoopTimeoutClassification(t *testing.T) {
	// 1. Uncaught error in setTimeout MUST report Phase: "runtime", NOT "canceled"
	_, errTimer := Execute(context.Background(), ExecRequest{
		Cwd:     t.TempDir(),
		Timeout: 2 * time.Second,
		Code: `
			await new Promise((_resolve) => {
				setTimeout(() => {
					throw new Error("uncaught timer failure");
				}, 5);
			});
		`,
	})
	var ee *ExecutionError
	if !errors.As(errTimer, &ee) || ee.Phase != "runtime" || !strings.Contains(ee.Message, "uncaught timer failure") {
		t.Fatalf("expected Phase=runtime for setTimeout uncaught error, got: %+v", errTimer)
	}

	// 2. `await sleep(5); while(true){}` MUST report Phase: "timeout", NEVER DeadlockError!
	_, errSpin := Execute(context.Background(), ExecRequest{
		Cwd:     t.TempDir(),
		Timeout: 80 * time.Millisecond,
		Code: `
			await sleep(5);
			while (true) {}
		`,
	})
	if !errors.As(errSpin, &ee) || ee.Phase != "timeout" || strings.Contains(ee.Message, "DeadlockError") {
		t.Fatalf("expected Phase=timeout (not DeadlockError) on post-await infinite loop, got: %+v", errSpin)
	}

	// 3. Genuine unresolved Promise with 0 active ops still reports DeadlockError quickly
	_, errDeadlock := Execute(context.Background(), ExecRequest{
		Cwd:     t.TempDir(),
		Timeout: 2 * time.Second,
		Code:    `await new Promise(() => {});`,
	})
	if !errors.As(errDeadlock, &ee) || ee.Phase != "runtime" || !strings.Contains(ee.Message, "DeadlockError") {
		t.Fatalf("expected runtime DeadlockError, got: %+v", errDeadlock)
	}
}

func TestTimersFIFOOrderCapAndCleanup(t *testing.T) {
	res, err := Execute(context.Background(), ExecRequest{
		Cwd: t.TempDir(),
		Code: `
			const order: number[] = [];
			await new Promise<void>((resolve) => {
				for (let i = 0; i < 25; i++) {
					const idx = i;
					setTimeout(() => {
						order.push(idx);
						if (order.length === 25) resolve();
					}, 0);
				}
			});
			// Also schedule a long unawaited background timer to verify cleanupTimers() stops it on exit
			setTimeout(() => {}, 60_000);
			return order;
		`,
	})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	got, ok := res.Value.([]any)
	if !ok || len(got) != 25 {
		t.Fatalf("expected 25 ordered items, got %v", res.Value)
	}
	for i := 0; i < 25; i++ {
		if got[i] != int64(i) {
			t.Fatalf("FIFO order violated at index %d: got %v", i, got)
		}
	}

	// Exceeding 1024 active timers must fail with RangeError
	_, errCap := Execute(context.Background(), ExecRequest{
		Cwd: t.TempDir(),
		Code: `
			for (let i = 0; i < 1030; i++) {
				setTimeout(() => {}, 60_000);
			}
		`,
	})
	var ee *ExecutionError
	if !errors.As(errCap, &ee) || ee.Phase != "runtime" || !strings.Contains(ee.Message, "1024") {
		t.Fatalf("expected active timer cap RangeError, got: %+v", errCap)
	}
}

func TestTypedArrayCloneIsolationExtractGoBytesOverflowAndValidUTF8Decode(t *testing.T) {
	binder := &regressionBinder{}
	res, err := Execute(context.Background(), ExecRequest{
		Cwd: t.TempDir(),
		Code: `
			const buf = Buffer.from([10, 20, 30, 40]);
			const p = (globalThis as any).testcap.inspectBytesAsync(buf);
			// Mutate backing buffer immediately BEFORE awaiting host op!
			buf[0] = 99;
			buf[1] = 88;
			await p;

			// Overflow guard: byteOffset=Infinity, byteLength=1 must not panic!
			const overflowDecoded = new TextDecoder().decode({
				buffer: new ArrayBuffer(8),
				byteOffset: Infinity,
				byteLength: 1,
			} as any);

			// Invalid UTF-8 sanitization
			const badBuf = Buffer.from([0xff, 0xfe, 0x41]).toString("utf8");
			const badTD = new TextDecoder().decode(new Uint8Array([0x80, 0x42]));
			return { overflowDecoded, badBuf, badTD };
		`,
	}, binder)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	binder.mu.Lock()
	captured := append([]byte(nil), binder.capturedBuf...)
	binder.mu.Unlock()
	if !bytes.Equal(captured, []byte{10, 20, 30, 40}) {
		t.Fatalf("async host op observed guest post-call mutation (missing bytes.Clone): got %v, want [10 20 30 40]", captured)
	}

	m := res.Value.(map[string]any)
	badBuf, _ := m["badBuf"].(string)
	badTD, _ := m["badTD"].(string)
	if !utf8.ValidString(badBuf) || !strings.Contains(badBuf, "\uFFFD") || !strings.HasSuffix(badBuf, "A") {
		t.Fatalf("Buffer.toString('utf8') did not sanitize invalid UTF-8: %q", badBuf)
	}
	if !utf8.ValidString(badTD) || badTD != "\uFFFDB" {
		t.Fatalf("TextDecoder.decode did not sanitize invalid UTF-8: %q", badTD)
	}
}

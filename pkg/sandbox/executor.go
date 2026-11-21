// Package sandbox executes compiled TypeScript programs inside isolated goja JavaScript VMs
// with concurrent host Promise bridging, operation budgets, and deadline enforcement.
package sandbox

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/aliothmoon/mcplex/pkg/compiler"
	"github.com/aliothmoon/mcplex/pkg/output"
	"github.com/dop251/goja"
)

const (
	DefaultTimeout = 20 * time.Second
	MaxTimeout     = 120 * time.Second
	DefaultMaxOps  = 400
	MaxLogEntries  = 2000
	MaxLogBytes    = 256 * 1024
)

// EffectiveDefaultTimeout returns DefaultTimeout (20s) unless overridden via AGY_PTC_DEFAULT_TIMEOUT_MS.
func EffectiveDefaultTimeout() time.Duration {
	if raw := strings.TrimSpace(os.Getenv("AGY_PTC_DEFAULT_TIMEOUT_MS")); raw != "" {
		if ms, err := strconv.Atoi(raw); err == nil && ms >= 500 {
			d := time.Duration(ms) * time.Millisecond
			if d > EffectiveMaxTimeout() {
				return EffectiveMaxTimeout()
			}
			return d
		}
	}
	return DefaultTimeout
}

// EffectiveMaxTimeout returns MaxTimeout (120s) unless overridden via AGY_PTC_MAX_TIMEOUT_MS.
func EffectiveMaxTimeout() time.Duration {
	if raw := strings.TrimSpace(os.Getenv("AGY_PTC_MAX_TIMEOUT_MS")); raw != "" {
		if ms, err := strconv.Atoi(raw); err == nil && ms >= 1000 && ms <= 600000 {
			return time.Duration(ms) * time.Millisecond
		}
	}
	return MaxTimeout
}

// OpTrace records a single host capability call executed by a guest program.
type OpTrace struct {
	Seq        int    `json:"seq"`
	Wave       int    `json:"wave,omitempty"` // parallel execution wave ID; ops sharing a Wave overlapped in flight (e.g. Promise.all)
	Capability string `json:"capability"`
	Target     string `json:"target,omitempty"`
	StartMs    int64  `json:"startMs,omitempty"` // offset in ms from exec start
	DurationMs int64  `json:"durationMs"`
	Status     string `json:"status"` // "ok" | "error"
	Kind       string `json:"kind,omitempty"`
	Error      string `json:"error,omitempty"`
	Bytes      int    `json:"bytes,omitempty"` // approximate size of the result handed to the program
}

// AsyncHostFunc is a thread-safe Go function invoked asynchronously from guest JS.
// Arguments have already been exported on the VM thread before running in a goroutine.
type AsyncHostFunc func(ctx context.Context, args []any) (any, error)

// SyncHostFunc runs synchronously on the VM thread.
type SyncHostFunc func(ctx context.Context, args []any) (any, error)

// Binder allows capability packages to register namespaces onto an ExecutionEnv.
type Binder interface {
	Bind(env *ExecutionEnv) error
}

// ExecRequest describes a single PTC program invocation.
type ExecRequest struct {
	Title     string
	Code      string
	Payloads  map[string]string
	Cwd       string
	Timeout   time.Duration
	MaxOps    int
	OnOpStart func(capability, target string, seq int)
}

// ExecResult contains the outcome, logs, and operation trace of a PTC execution.
type ExecResult struct {
	Value      any       `json:"value"`
	HasValue   bool      `json:"hasValue,omitempty"`
	Logs       []string  `json:"logs,omitempty"`
	Warnings   []string  `json:"warnings,omitempty"`
	Traces     []OpTrace `json:"traces,omitempty"`
	DurationMs int64     `json:"durationMs"`
}

// ExecutionError wraps compile or runtime failures along with partial logs and completed traces.
type ExecutionError struct {
	Phase       string // "compile" | "runtime" | "timeout"
	Message     string
	TimedOutOps []string
	Logs        []string
	Warnings    []string
	Traces      []OpTrace
	DurationMs  int64
}

func (e *ExecutionError) Error() string {
	return e.Message
}

type loopAction struct {
	run func()
}

type inFlightEntry struct {
	seq        int
	capability string
	target     string
	startedAt  time.Time
}

var sandboxExecSeq atomic.Int64

// ExecutionEnv represents one isolated VM run and provides safe host binding helpers.
type ExecutionEnv struct {
	ExecID        string
	Ctx           context.Context
	Cwd           string
	VM            *goja.Runtime
	loopCh        chan loopAction
	deliverFn     goja.Callable
	makeErrFn     goja.Callable
	opSeq         atomic.Int32
	maxOps        int
	onOpStart     func(capability, target string, seq int)
	tracesMu      sync.Mutex
	traces        []OpTrace
	inFlightMu    sync.Mutex
	inFlight      map[int]inFlightEntry
	asyncWG       sync.WaitGroup
	logsMu        sync.Mutex
	logs          []string
	logBytes      int
	pendingOps    atomic.Int32
	vmActiveAsync int // VM-thread only: count of unsettled async ops for exact Promise.all wave grouping
	waveSeq       int // VM-thread only: increments whenever a new serial step or parallel wave starts
	startedAt     time.Time
	cleanupFns    []func()
	warnMu        sync.Mutex
	warnings      []string
	warnCount     map[string]int
	compiled      *compiler.CompileResult
	asyncErr      error // VM-thread only: first exception thrown outside the main promise chain (timer callbacks)
}

// RegisterCleanup registers a function to run when the VM execution finishes (e.g. rolling back uncommitted transactions).
// Cleanup functions may call AddWarning; warnings are attached to the returned ExecResult / ExecutionError.
func (env *ExecutionEnv) RegisterCleanup(fn func()) {
	env.cleanupFns = append(env.cleanupFns, fn)
}

// AddWarning records a host-side notice that must reach the model even when the program succeeded
// (e.g. an uncommitted transaction was auto-rolled back, or a rollback could not restore a file).
// Identical messages are collapsed into one line with a `(×N)` suffix.
func (env *ExecutionEnv) AddWarning(msg string) {
	env.warnMu.Lock()
	defer env.warnMu.Unlock()
	if env.warnCount == nil {
		env.warnCount = map[string]int{}
	}
	if env.warnCount[msg] == 0 {
		env.warnings = append(env.warnings, msg)
	}
	env.warnCount[msg]++
}

func (env *ExecutionEnv) getWarnings() []string {
	env.warnMu.Lock()
	defer env.warnMu.Unlock()
	if len(env.warnings) == 0 {
		return nil
	}
	out := make([]string, len(env.warnings))
	for i, w := range env.warnings {
		if n := env.warnCount[w]; n > 1 {
			w = fmt.Sprintf("%s (×%d)", w, n)
		}
		out[i] = w
	}
	return out
}

// userCallSite returns the user source line of the innermost user frame on the current JS stack (0 if none).
// Host calls are invoked synchronously from guest code, so this pins async failures to the line that issued them.
func (env *ExecutionEnv) userCallSite() int {
	if env.compiled == nil {
		return 0
	}
	for _, f := range env.VM.CaptureCallStack(8, nil) {
		if f.SrcName() == compiler.ProgramName {
			return env.compiled.UserLine(f.Position().Line, f.Position().Column)
		}
	}
	return 0
}

// hostFailure converts a Go error into a trace entry and a guest JS error object.
func (env *ExecutionEnv) hostFailure(seq int, capability, target string, dur int64, line int, err error) (OpTrace, func() goja.Value) {
	kind, msg, data := env.classifyHostError(err)
	tr := OpTrace{Seq: seq, Capability: capability, Target: target, DurationMs: dur, Status: "error", Kind: kind, Error: msg}
	return tr, func() goja.Value { return env.newJSHostError(kind, capability, msg, line, data) }
}

// SetNamespace creates or replaces a global object with the given async and sync methods.
func (env *ExecutionEnv) SetNamespace(ns string, asyncMethods map[string]AsyncHostFunc, syncMethods map[string]SyncHostFunc) error {
	obj := env.VM.NewObject()
	for name, fn := range asyncMethods {
		fullCap := PublicOpName(ns + "." + name)
		bound := env.wrapAsync(fullCap, fn)
		if err := obj.Set(name, bound); err != nil {
			return err
		}
	}
	for name, fn := range syncMethods {
		fullCap := PublicOpName(ns + "." + name)
		bound := env.wrapSync(fullCap, fn)
		if err := obj.Set(name, bound); err != nil {
			return err
		}
	}
	return env.VM.Set(ns, obj)
}

func (env *ExecutionEnv) wrapSync(capability string, fn SyncHostFunc) func(goja.FunctionCall) goja.Value {
	return func(call goja.FunctionCall) goja.Value {
		seq := int(env.opSeq.Add(1))
		line := env.userCallSite()
		if seq > env.maxOps {
			panic(env.newJSHostError(KindError, capability, opBudgetMessage(env.maxOps), line, nil))
		}
		exported := exportArgs(call.Arguments)
		target := summarizeTarget(exported)
		if env.onOpStart != nil {
			env.onOpStart(capability, target, seq)
		}
		if env.vmActiveAsync == 0 {
			env.waveSeq++
		}
		wave := env.waveSeq
		start := time.Now()
		startMs := start.Sub(env.startedAt).Milliseconds()
		res, err := fn(env.Ctx, exported)
		dur := time.Since(start).Milliseconds()
		if err != nil {
			tr, jsErr := env.hostFailure(seq, capability, target, dur, line, err)
			tr.Wave = wave
			tr.StartMs = startMs
			env.recordTrace(tr)
			panic(jsErr())
		}
		env.recordTrace(OpTrace{Seq: seq, Wave: wave, Capability: capability, Target: target, StartMs: startMs, DurationMs: dur, Status: "ok", Bytes: approxSize(res)})
		return env.toJSValue(res)
	}
}

func (env *ExecutionEnv) wrapAsync(capability string, fn AsyncHostFunc) func(goja.FunctionCall) goja.Value {
	return func(call goja.FunctionCall) goja.Value {
		seq := int(env.opSeq.Add(1))
		line := env.userCallSite()
		if seq > env.maxOps {
			promise, _, reject := env.VM.NewPromise()
			errVal := env.newJSHostError(KindError, capability, opBudgetMessage(env.maxOps), line, nil)
			_ = reject(errVal)
			_, _ = env.deliverFn(goja.Undefined())
			return env.VM.ToValue(promise)
		}
		exported := exportArgs(call.Arguments)
		target := summarizeTarget(exported)
		if env.onOpStart != nil {
			env.onOpStart(capability, target, seq)
		}

		promise, resolve, reject := env.VM.NewPromise()
		if env.vmActiveAsync == 0 {
			env.waveSeq++
		}
		wave := env.waveSeq
		env.vmActiveAsync++
		env.pendingOps.Add(1)
		env.asyncWG.Add(1)
		start := time.Now()
		startMs := start.Sub(env.startedAt).Milliseconds()
		env.trackInFlightStart(seq, capability, target, start)

		go func() {
			defer env.asyncWG.Done()
			res, err := fn(env.Ctx, exported)
			dur := time.Since(start).Milliseconds()
			env.trackInFlightDone(seq)
			var jsErr func() goja.Value
			if err != nil {
				var tr OpTrace
				tr, jsErr = env.hostFailure(seq, capability, target, dur, line, err)
				tr.Wave = wave
				tr.StartMs = startMs
				env.recordTrace(tr)
			} else {
				env.recordTrace(OpTrace{Seq: seq, Wave: wave, Capability: capability, Target: target, StartMs: startMs, DurationMs: dur, Status: "ok", Bytes: approxSize(res)})
			}

			select {
			case env.loopCh <- loopAction{
				run: func() {
					defer env.pendingOps.Add(-1)
					// Decrement vmActiveAsync BEFORE settling the Promise so a sequential `await`
					// resuming inside deliverFn starts a fresh wave instead of joining this one.
					if env.vmActiveAsync > 0 {
						env.vmActiveAsync--
					}
					if err != nil {
						_ = reject(jsErr())
						_, _ = env.deliverFn(goja.Undefined())
						return
					}
					val := env.toJSValue(res)
					_ = resolve(val)
					_, _ = env.deliverFn(goja.Undefined())
				},
			}:
			case <-env.Ctx.Done():
				env.pendingOps.Add(-1)
			}
		}()

		return env.VM.ToValue(promise)
	}
}

func (env *ExecutionEnv) trackInFlightStart(seq int, capability, target string, startedAt time.Time) {
	env.inFlightMu.Lock()
	defer env.inFlightMu.Unlock()
	if env.inFlight == nil {
		env.inFlight = map[int]inFlightEntry{}
	}
	env.inFlight[seq] = inFlightEntry{seq: seq, capability: capability, target: target, startedAt: startedAt}
}

func (env *ExecutionEnv) trackInFlightDone(seq int) {
	env.inFlightMu.Lock()
	defer env.inFlightMu.Unlock()
	delete(env.inFlight, seq)
}

func (env *ExecutionEnv) snapshotInFlight() []string {
	env.inFlightMu.Lock()
	defer env.inFlightMu.Unlock()
	if len(env.inFlight) == 0 {
		return nil
	}
	items := make([]inFlightEntry, 0, len(env.inFlight))
	for _, v := range env.inFlight {
		items = append(items, v)
	}
	sort.Slice(items, func(i, j int) bool { return items[i].seq < items[j].seq })
	now := time.Now()
	out := make([]string, len(items))
	for i, it := range items {
		label := fmt.Sprintf("#%d %s", it.seq, it.capability)
		if it.target != "" {
			label += "(" + it.target + ")"
		}
		out[i] = fmt.Sprintf("%s [running %dms]", label, now.Sub(it.startedAt).Milliseconds())
	}
	return out
}

func (env *ExecutionEnv) waitAsyncDrain(maxWait time.Duration) {
	done := make(chan struct{})
	go func() {
		env.asyncWG.Wait()
		close(done)
	}()
	t := time.NewTimer(maxWait)
	defer t.Stop()
	select {
	case <-done:
	case <-t.C:
	}
}

func (env *ExecutionEnv) toJSValue(v any) goja.Value {
	if v == nil {
		return goja.Null()
	}
	if om, ok := v.(*OrderedMap); ok {
		return env.orderedToJS(om)
	}
	// Round-trip structs via JSON into clean plain JS objects/arrays so Object.keys, destructuring, and camelCase JSON tags work seamlessly in TS/JS.
	switch v.(type) {
	case string, bool, int, int64, float64, map[string]any, []any:
		return env.VM.ToValue(v)
	default:
		raw, err := json.Marshal(v)
		if err != nil {
			return env.VM.ToValue(v)
		}
		var generic any
		if err := json.Unmarshal(raw, &generic); err != nil {
			return env.VM.ToValue(v)
		}
		return env.VM.ToValue(generic)
	}
}

func (env *ExecutionEnv) recordTrace(t OpTrace) {
	env.tracesMu.Lock()
	defer env.tracesMu.Unlock()
	env.traces = append(env.traces, t)
}

func (env *ExecutionEnv) getTraces() []OpTrace {
	env.tracesMu.Lock()
	defer env.tracesMu.Unlock()
	cp := make([]OpTrace, len(env.traces))
	copy(cp, env.traces)
	return cp
}

func (env *ExecutionEnv) appendLog(level string, args []goja.Value) {
	env.logsMu.Lock()
	defer env.logsMu.Unlock()
	if len(env.logs) >= MaxLogEntries || env.logBytes >= MaxLogBytes {
		return
	}
	parts := make([]string, len(args))
	for i, a := range args {
		if a == nil || goja.IsUndefined(a) {
			parts[i] = "undefined"
			continue
		}
		if goja.IsNull(a) {
			parts[i] = "null"
			continue
		}
		if obj, ok := a.(*goja.Object); ok && obj.ClassName() == "Error" {
			parts[i] = a.String()
			continue
		}
		exp := a.Export()
		switch val := exp.(type) {
		case string:
			parts[i] = val
		default:
			var buf bytes.Buffer
			enc := json.NewEncoder(&buf)
			enc.SetEscapeHTML(false)
			if err := enc.Encode(val); err == nil {
				parts[i] = strings.TrimRight(buf.String(), "\n")
			} else {
				parts[i] = fmt.Sprintf("%v", val)
			}
		}
	}
	line := strings.Join(parts, " ")
	if level != "" && level != "log" {
		line = "[" + level + "] " + line
	}
	remaining := MaxLogBytes - env.logBytes
	if len(line) > remaining {
		line = output.SliceUTF8Bytes(line, remaining) + "...(truncated)"
	}
	env.logs = append(env.logs, line)
	env.logBytes += len(line) + 1
}

func (env *ExecutionEnv) getLogs() []string {
	env.logsMu.Lock()
	defer env.logsMu.Unlock()
	cp := make([]string, len(env.logs))
	copy(cp, env.logs)
	return cp
}

// Execute compiles TypeScript code, boots a fresh isolated goja VM, binds capabilities, and awaits completion.
func Execute(parentCtx context.Context, req ExecRequest, binders ...Binder) (result *ExecResult, retErr error) {
	start := time.Now()
	cr, err := compiler.Compile(req.Code, req.Payloads)
	if err != nil {
		return nil, &ExecutionError{
			Phase:      "compile",
			Message:    err.Error(),
			DurationMs: time.Since(start).Milliseconds(),
		}
	}

	timeout := req.Timeout
	if timeout <= 0 {
		timeout = EffectiveDefaultTimeout()
	}
	maxTimeout := EffectiveMaxTimeout()
	if timeout > maxTimeout {
		timeout = maxTimeout
	}
	maxOps := req.MaxOps
	if maxOps <= 0 {
		maxOps = DefaultMaxOps
	}

	ctx, cancel := context.WithTimeout(parentCtx, timeout)

	vm := goja.New()
	vm.SetFieldNameMapper(goja.TagFieldNameMapper("json", true))

	env := &ExecutionEnv{
		ExecID:    fmt.Sprintf("exec:%d", sandboxExecSeq.Add(1)),
		Ctx:       ctx,
		Cwd:       req.Cwd,
		VM:        vm,
		loopCh:    make(chan loopAction, 256),
		maxOps:    maxOps,
		onOpStart: req.OnOpStart,
		inFlight:  map[int]inFlightEntry{},
		startedAt: start,
		compiled:  cr,
	}
	// Crucial teardown order on exit:
	// 1) cancel() kills all child process trees via cmd.Cancel()
	// 2) waitAsyncDrain(800ms) waits for subprocess handles to release
	// 3) cleanupFns roll back uncommitted tx.* file snapshots safely without Windows file-lock contention!
	defer func() {
		cancel()
		env.waitAsyncDrain(800 * time.Millisecond)
		for i := len(env.cleanupFns) - 1; i >= 0; i-- {
			env.cleanupFns[i]()
		}
		if ws := env.getWarnings(); len(ws) > 0 {
			if result != nil {
				result.Warnings = ws
			}
			var ee *ExecutionError
			if errors.As(retErr, &ee) {
				ee.Warnings = ws
			}
		}
	}()

	// Watchdog goroutine interrupts infinite synchronous CPU loops in JS (`while(true){}`) immediately on deadline.
	doneWatch := make(chan struct{})
	defer close(doneWatch)
	go func() {
		select {
		case <-ctx.Done():
			if parentCtx.Err() != nil && !errors.Is(parentCtx.Err(), context.DeadlineExceeded) {
				vm.Interrupt("execution canceled")
			} else {
				vm.Interrupt("execution deadline exceeded")
			}
		case <-doneWatch:
		}
	}()

	deliverVal, err := vm.RunString(`(function __flushMicrotasks() {})`)
	if err != nil {
		return nil, fmt.Errorf("bootstrap deliver failed: %w", err)
	}
	deliverFn, ok := goja.AssertFunction(deliverVal)
	if !ok {
		return nil, errors.New("bootstrap deliver is not callable")
	}
	env.deliverFn = deliverFn

	// Guest-visible host error: `name` = semantic kind, `message` = "op: detail" (one line), `data` = optional details.
	// Enumerable {name, message, data} so returned/logged errors serialize compactly; toJSON yields the same one-liner.
	// `stack` points at the user line that issued the call instead of this internal factory frame.
	makeErrVal, err := vm.RunString(`(function __makeHostError(name, op, msg, line, data) {
		const e = new Error(op + ": " + msg);
		Object.defineProperty(e, "name", { value: name, enumerable: true, writable: true, configurable: true });
		Object.defineProperty(e, "message", { value: e.message, enumerable: true, writable: true, configurable: true });
		Object.defineProperty(e, "op", { value: op, writable: true, configurable: true });
		if (data) e.data = data;
		Object.defineProperty(e, "stack", { value: name + ": " + e.message + (line > 0 ? "\n    at code:" + line : ""), writable: true, configurable: true });
		Object.defineProperty(e, "toJSON", { value: function () { return this.name + ": " + this.message; }, configurable: true });
		return e;
	})`)
	if err != nil {
		return nil, fmt.Errorf("bootstrap makeHostError failed: %w", err)
	}
	makeErrFn, ok := goja.AssertFunction(makeErrVal)
	if !ok {
		return nil, errors.New("bootstrap makeHostError is not callable")
	}
	env.makeErrFn = makeErrFn

	if err := installConsoleAndTimers(env); err != nil {
		return nil, err
	}
	if err := installStandardGlobals(env); err != nil {
		return nil, err
	}
	if err := installTimers(env); err != nil {
		return nil, err
	}
	if err := installProcessOS(env); err != nil {
		return nil, err
	}

	for _, b := range binders {
		if err := b.Bind(env); err != nil {
			return nil, fmt.Errorf("bind capability failed: %w", err)
		}
	}

	if err := installBootstrapWrappers(vm); err != nil {
		return nil, err
	}

	payloadProxy, err := buildPayloadProxy(vm, req.Payloads)
	if err != nil {
		return nil, err
	}
	_ = vm.Set("π", payloadProxy)

	prog, err := goja.Compile("user_program.ts", cr.JSCode, false)
	if err != nil {
		return nil, &ExecutionError{
			Phase:      "compile",
			Message:    compiler.FormatRuntimeError(err.Error(), cr),
			DurationMs: time.Since(start).Milliseconds(),
		}
	}

	fnVal, err := vm.RunProgram(prog)
	if err != nil {
		return nil, env.wrapRuntimeErr(err, cr, start)
	}
	entryFn, ok := goja.AssertFunction(fnVal)
	if !ok {
		return nil, &ExecutionError{
			Phase:      "runtime",
			Message:    "internal error: compiled wrapper did not produce an entry function",
			DurationMs: time.Since(start).Milliseconds(),
		}
	}

	retVal, err := entryFn(goja.Undefined(), payloadProxy, payloadProxy)
	if err != nil {
		return nil, env.wrapRuntimeErr(err, cr, start)
	}

	promise, ok := retVal.Export().(*goja.Promise)
	if !ok {
		hasVal := retVal != nil && !goja.IsUndefined(retVal)
		var exported any
		if hasVal {
			exported = retVal.Export()
		}
		return &ExecResult{
			Value:      exported,
			HasValue:   hasVal,
			Logs:       env.getLogs(),
			Traces:     env.getTraces(),
			DurationMs: time.Since(start).Milliseconds(),
		}, nil
	}

	for promise.State() == goja.PromiseStatePending {
		if env.pendingOps.Load() == 0 && len(env.loopCh) == 0 {
			cancel()
			return nil, &ExecutionError{
				Phase:      "runtime",
				Message:    "DeadlockError: top-level Promise remained pending with 0 active host operations (check for unresolved new Promise())",
				Logs:       env.getLogs(),
				Traces:     env.getTraces(),
				DurationMs: time.Since(start).Milliseconds(),
			}
		}
		select {
		case act := <-env.loopCh:
			act.run()
			if env.asyncErr != nil {
				cancel()
				return nil, env.wrapRuntimeErr(env.asyncErr, cr, start)
			}
		case <-ctx.Done():
			snapInFlight := env.snapshotInFlight()
			cancel()
			env.waitAsyncDrain(800 * time.Millisecond)
			stillStuck := env.snapshotInFlight()
			culprits := stillStuck
			if len(culprits) == 0 {
				traces := env.getTraces()
				recordedErr := false
				for _, tr := range traces {
					if tr.Status == "error" {
						recordedErr = true
						break
					}
				}
				if !recordedErr {
					culprits = snapInFlight
				}
			}
			if parentCtx.Err() != nil && !errors.Is(parentCtx.Err(), context.DeadlineExceeded) {
				return nil, &ExecutionError{
					Phase:       "canceled",
					Message:     "Canceled: execution canceled by client",
					TimedOutOps: culprits,
					Logs:        env.getLogs(),
					Traces:      env.getTraces(),
					DurationMs:  time.Since(start).Milliseconds(),
				}
			}
			return nil, &ExecutionError{
				Phase:       "timeout",
				Message:     fmt.Sprintf("Timeout: program exceeded %s (timeoutMs=%d, max %d)", timeout, timeout.Milliseconds(), EffectiveMaxTimeout().Milliseconds()),
				TimedOutOps: culprits,
				Logs:        env.getLogs(),
				Traces:      env.getTraces(),
				DurationMs:  time.Since(start).Milliseconds(),
			}
		}
	}

	if promise.State() == goja.PromiseStateRejected {
		rej := promise.Result()
		msg := formatRejectedValue(vm, rej)
		cancel()
		env.waitAsyncDrain(800 * time.Millisecond)
		traces := env.getTraces()
		if msg == "" || msg == "[object Object]" || msg == "{}" {
			for i := len(traces) - 1; i >= 0; i-- {
				if traces[i].Status == "error" && traces[i].Error != "" {
					msg = fmt.Sprintf("%s: %s", traces[i].Capability, traces[i].Error)
					break
				}
			}
		}
		formattedMsg := compiler.FormatRuntimeError(msg, cr)
		formattedMsg = enrichUnknownMethodError(vm, formattedMsg, cr)
		formattedMsg = addTypeHint(formattedMsg)
		formattedMsg = compiler.CleanStack(formattedMsg, cr)
		return nil, &ExecutionError{
			Phase:      "runtime",
			Message:    formattedMsg,
			Logs:       env.getLogs(),
			Traces:     traces,
			DurationMs: time.Since(start).Milliseconds(),
		}
	}

	var exported any
	hasVal := false
	if res := promise.Result(); res != nil && !goja.IsUndefined(res) {
		hasVal = true
		exported = res.Export()
	}
	return &ExecResult{
		Value:      exported,
		HasValue:   hasVal,
		Logs:       env.getLogs(),
		Traces:     env.getTraces(),
		DurationMs: time.Since(start).Milliseconds(),
	}, nil
}

func (env *ExecutionEnv) wrapRuntimeErr(err error, cr *compiler.CompileResult, start time.Time) error {
	phase := "runtime"
	var interrupted *goja.InterruptedError
	if errors.As(err, &interrupted) {
		if strings.Contains(strings.ToLower(interrupted.String()), "canceled") {
			phase = "canceled"
		} else {
			phase = "timeout"
		}
	} else if errors.Is(env.Ctx.Err(), context.Canceled) {
		phase = "canceled"
	} else if errors.Is(env.Ctx.Err(), context.DeadlineExceeded) {
		phase = "timeout"
	}
	formattedMsg := compiler.FormatRuntimeError(err.Error(), cr)
	if phase == "runtime" {
		formattedMsg = enrichUnknownMethodError(env.VM, formattedMsg, cr)
		formattedMsg = addTypeHint(formattedMsg)
	}
	formattedMsg = compiler.CleanStack(formattedMsg, cr)
	return &ExecutionError{
		Phase:       phase,
		Message:     formattedMsg,
		TimedOutOps: env.snapshotInFlight(),
		Logs:        env.getLogs(),
		Traces:      env.getTraces(),
		DurationMs:  time.Since(start).Milliseconds(),
	}
}

func installConsoleAndTimers(env *ExecutionEnv) error {
	consoleObj := env.VM.NewObject()
	for _, lvl := range []string{"log", "info", "warn", "error", "debug"} {
		level := lvl
		_ = consoleObj.Set(level, func(call goja.FunctionCall) goja.Value {
			env.appendLog(level, call.Arguments)
			return goja.Undefined()
		})
	}
	if err := env.VM.Set("console", consoleObj); err != nil {
		return err
	}
	if err := env.VM.Set("print", func(call goja.FunctionCall) goja.Value {
		env.appendLog("log", call.Arguments)
		return goja.Undefined()
	}); err != nil {
		return err
	}
	return env.VM.Set("sleep", env.wrapAsync("sleep", func(ctx context.Context, args []any) (any, error) {
		ms := 50
		if len(args) > 0 {
			switch v := args[0].(type) {
			case int64:
				ms = int(v)
			case float64:
				ms = int(v)
			case int:
				ms = v
			}
		}
		if ms < 0 {
			ms = 0
		}
		if ms > 30000 {
			ms = 30000
		}
		t := time.NewTimer(time.Duration(ms) * time.Millisecond)
		defer t.Stop()
		select {
		case <-t.C:
			return nil, nil
		case <-ctx.Done():
			return nil, ctx.Err()
		}
	}))
}

func buildPayloadProxy(vm *goja.Runtime, payloads map[string]string) (goja.Value, error) {
	raw := map[string]any{}
	for k, v := range payloads {
		raw[k] = v
	}
	_ = vm.Set("__rawPayloads", raw)
	proxyVal, err := vm.RunString(`(function() {
		const target = Object.freeze(Object.assign({}, __rawPayloads));
		delete globalThis.__rawPayloads;
		const keys = Object.keys(target);
		return new Proxy(target, {
			get(t, prop) {
				if (typeof prop === "symbol" || prop === "then" || prop === "toJSON" || prop in Object.prototype) {
					return t[prop];
				}
				if (!(prop in t)) {
					throw new Error("PayloadError: key '" + String(prop) + "' does not exist in payloads. Provided keys: " + (keys.length ? keys.join(", ") : "(none)"));
				}
				return t[prop];
			}
		});
	})()`)
	return proxyVal, err
}

// newJSHostError creates a genuine JavaScript Error instance with .message, .stack, and .toJSON()
// so both uncaught rejections and `catch (e) { JSON.stringify(e) }` produce clear diagnostic strings instead of `[object Object]` or `{}`.
func (env *ExecutionEnv) newJSHostError(kind, capability, msg string, line int, data map[string]any) goja.Value {
	if env.makeErrFn != nil {
		dataVal := goja.Undefined()
		if len(data) > 0 {
			dataVal = env.toJSValue(data)
		}
		if val, err := env.makeErrFn(goja.Undefined(), env.VM.ToValue(kind), env.VM.ToValue(capability), env.VM.ToValue(msg), env.VM.ToValue(line), dataVal); err == nil {
			return val
		}
	}
	return env.VM.ToValue(kind + ": " + capability + ": " + msg)
}

func opBudgetMessage(maxOps int) string {
	return fmt.Sprintf("op budget exceeded (%d host calls); batch work (e.g. ast.outlines, sh.checks) or split the program", maxOps)
}

// installBootstrapWrappers adds ergonomic JS facades over raw host namespaces:
// 1. `mcp.<server>.<tool>(args)` dynamic Proxy routing to `mcp.call("server/tool", args)`
// 2. `tx.begin()` returning a first-class handle `{ id, edit, write, verify, commit, rollback }`
// 3. Common LLM method aliases (`fs.readFile` -> `fs.read`, `search.grep` -> `search.rg`, etc.) + clear `UnknownMethodError` listing valid methods!
func installBootstrapWrappers(vm *goja.Runtime) error {
	_, err := vm.RunString(`(function() {
		if (typeof globalThis.mcp === "object" && globalThis.mcp !== null) {
			const baseMcp = globalThis.mcp;
			globalThis.mcp = new Proxy(baseMcp, {
				get(target, serverProp) {
					if (typeof serverProp !== "string" || serverProp in target || serverProp === "then") {
						return target[serverProp];
					}
					return new Proxy({}, {
						get(_, toolProp) {
							if (typeof toolProp !== "string" || toolProp === "then") return undefined;
							return (args) => baseMcp.call(serverProp + "/" + toolProp, args || {});
						}
					});
				}
			});
		}
		if (typeof globalThis.fs === "object" && globalThis.fs !== null && typeof globalThis.fs._bytes === "function") {
			const rawBytes = globalThis.fs._bytes;
			globalThis.fs.bytes = async (path, opts) => Buffer.from(await rawBytes(path, opts || {}), "base64");
		}
		if (typeof globalThis.tx === "object" && globalThis.tx !== null && typeof globalThis.tx._begin === "function") {
			const rawTx = globalThis.tx;
			globalThis.tx = {
				begin: async () => {
					const info = await rawTx._begin();
					const id = info.id;
					return {
						id,
						edit: (pathOrSpec, hunks) => rawTx._edit(id, pathOrSpec, hunks),
						write: (path, content, opts) => rawTx._write(id, path, content, opts || {}),
						rm: (path) => rawTx._rm(id, path),
						remove: (path) => rawTx._rm(id, path),
						rename: (from, to) => rawTx._rename(id, from, to),
						mv: (from, to) => rawTx._rename(id, from, to),
						verify: (commands, opts) => rawTx._verify(id, commands, opts || {}),
						commit: () => rawTx._commit(id),
						rollback: () => rawTx._rollback(id),
						diff: () => rawTx._diff(id),
						status: () => rawTx._status(id),
					};
				}
			};
		}

		const aliasMap = {
			fs: { readFile: "read", writeFile: "write", readdir: "list", ls: "list", dir: "list", readMany: "cat", readFiles: "cat",
				mv: "rename", move: "rename", copy: "cp", copyFile: "cp", unlink: "rm", remove: "rm", mkdirp: "mkdir",
				readBytes: "bytes", readBinary: "bytes", readBuffer: "bytes", checksum: "hash", digest: "hash" },
			search: { grep: "rg", find: "glob", files: "glob" },
			ast: { symbols: "outline", parse: "outline", readSymbol: "read", symbol: "read", getSymbol: "read" },
			edit: { edit: "apply", patch: "apply" },
			sh: { bash: "exec", pwsh: "exec", check: "checks", execFile: "run", spawn: "run" },
			state: { load: "get", store: "set", del: "delete", remove: "delete" },
		};

		globalThis.__ptcLastMissingMethod = null;
		for (const ns of ["fs", "search", "ast", "edit", "tx", "git", "sh", "state", "tools"]) {
			const base = globalThis[ns];
			if (typeof base !== "object" || base === null) continue;
			const aliases = aliasMap[ns] || {};
			const validKeys = Object.keys(base).filter((k) => !k.startsWith("_")).sort();
			globalThis[ns] = new Proxy(base, {
				get(t, prop) {
					if (typeof prop !== "string" || prop === "then" || prop === "toJSON" || prop in Object.prototype) {
						return t[prop];
					}
					if (prop in t) return t[prop];
					if (prop in aliases) return t[aliases[prop]];
					globalThis.__ptcLastMissingMethod = { ns, prop, validKeys };
					return undefined;
				}
			});
		}
	})()`)
	return err
}

var (
	gojaNoMemberRegex  = regexp.MustCompile(`TypeError:\s+Object has no member '([^']+)'`)
	gojaNotFuncRegex   = regexp.MustCompile(`TypeError:\s+(?:Value is not an object:\s*undefined|Value is not a function|Property '([^']+)' of object .* is not a function|([^ \n]+) is not a function)`)
	codeStackLineRegex = regexp.MustCompile(`code:(\d+):\d+`)
)

// enrichUnknownMethodError upgrades Goja's generic TypeError on a missing PTC namespace method call
// (e.g. `sh.tree(...)` or `fs.totallyMadeUpMethod(...)`) into an actionable UnknownMethodError,
// while allowing non-throwing feature checks like `fs.search ? ... : ...` to evaluate to undefined.
func enrichUnknownMethodError(vm *goja.Runtime, formattedMsg string, cr *compiler.CompileResult) string {
	if vm == nil || !strings.Contains(formattedMsg, "TypeError:") {
		return formattedMsg
	}
	val := vm.Get("__ptcLastMissingMethod")
	if val == nil || goja.IsUndefined(val) || goja.IsNull(val) {
		return formattedMsg
	}
	m, ok := val.Export().(map[string]any)
	if !ok {
		return formattedMsg
	}
	ns, _ := m["ns"].(string)
	prop, _ := m["prop"].(string)
	if ns == "" || prop == "" {
		return formattedMsg
	}
	var validKeys []string
	switch vk := m["validKeys"].(type) {
	case []any:
		for _, item := range vk {
			if s, ok := item.(string); ok {
				validKeys = append(validKeys, s)
			}
		}
	case []string:
		validKeys = vk
	}

	matched := false
	if sub := gojaNoMemberRegex.FindStringSubmatch(formattedMsg); len(sub) >= 2 {
		matched = sub[1] == prop
	} else if sub := gojaNotFuncRegex.FindStringSubmatch(formattedMsg); len(sub) >= 1 {
		if sub[1] != "" {
			matched = sub[1] == prop
		} else if sub[2] != "" {
			matched = sub[2] == prop || strings.HasSuffix(sub[2], "."+prop)
		} else {
			matched = true
		}
	}
	if !matched {
		return formattedMsg
	}
	// Guard against unrelated TypeErrors sharing the same property name by confirming the failing line references `ns` and `prop`.
	if cr != nil && len(cr.SourceLine) > 0 {
		if loc := codeStackLineRegex.FindStringSubmatch(formattedMsg); len(loc) >= 2 {
			if lineNo, err := strconv.Atoi(loc[1]); err == nil && lineNo >= 1 && lineNo <= len(cr.SourceLine) {
				srcLine := cr.SourceLine[lineNo-1]
				if !strings.Contains(srcLine, ns) || !strings.Contains(srcLine, prop) {
					return formattedMsg
				}
			}
		}
	}
	header := fmt.Sprintf("Error: UnknownMethodError: '%s.%s' does not exist. Available methods on '%s': %s", ns, prop, ns, strings.Join(validKeys, ", "))
	if nl := strings.IndexByte(formattedMsg, '\n'); nl >= 0 {
		return header + formattedMsg[nl:]
	}
	return header
}

func formatRejectedValue(vm *goja.Runtime, rej goja.Value) string {
	if rej == nil || goja.IsUndefined(rej) || goja.IsNull(rej) {
		return "Promise rejected with undefined"
	}
	if obj := rej.ToObject(vm); obj != nil {
		var msgStr, stackStr string
		if m := obj.Get("message"); m != nil && !goja.IsUndefined(m) && !goja.IsNull(m) {
			msgStr = strings.TrimSpace(m.String())
		}
		if s := obj.Get("stack"); s != nil && !goja.IsUndefined(s) && !goja.IsNull(s) {
			stackStr = strings.TrimSpace(s.String())
		}
		if stackStr != "" {
			if msgStr != "" && !strings.Contains(stackStr, msgStr) {
				return msgStr + "\n" + stackStr
			}
			return stackStr
		}
		if msgStr != "" {
			return msgStr
		}
		if b, err := json.Marshal(rej.Export()); err == nil && string(b) != "{}" && string(b) != "null" {
			return string(b)
		}
	}
	s := rej.String()
	if s == "[object Object]" {
		if b, err := json.Marshal(rej.Export()); err == nil {
			return string(b)
		}
	}
	return s
}

func exportArgs(vals []goja.Value) []any {
	out := make([]any, len(vals))
	for i, v := range vals {
		if v == nil || goja.IsUndefined(v) || goja.IsNull(v) {
			out[i] = nil
		} else {
			out[i] = v.Export()
		}
	}
	return out
}

func summarizeTarget(args []any) string {
	if len(args) == 0 || args[0] == nil {
		return ""
	}
	switch v := args[0].(type) {
	case string:
		if len(v) > 80 {
			return output.SliceUTF8Bytes(v, 80) + "..."
		}
		return v
	case map[string]any:
		for _, k := range []string{"path", "pattern", "command", "query", "ref", "name"} {
			if s, ok := v[k].(string); ok && s != "" {
				if len(s) > 80 {
					return output.SliceUTF8Bytes(s, 80) + "..."
				}
				return s
			}
		}
	}
	return ""
}

var noMemberRegex = regexp.MustCompile(`Object has no member '([A-Za-z_$][\w$]*)'`)

var (
	stringOnlyMethods = map[string]bool{
		"endsWith": true, "startsWith": true, "split": true, "trim": true, "trimStart": true, "trimEnd": true,
		"toLowerCase": true, "toUpperCase": true, "replace": true, "replaceAll": true, "match": true, "matchAll": true,
		"padStart": true, "padEnd": true, "charAt": true, "charCodeAt": true, "codePointAt": true, "substring": true,
		"substr": true, "localeCompare": true, "normalize": true, "repeat": true, "search": true,
	}
	arrayOnlyMethods = map[string]bool{
		"map": true, "filter": true, "forEach": true, "find": true, "findIndex": true, "some": true, "every": true,
		"reduce": true, "flatMap": true, "join": true, "push": true, "sort": true, "reverse": true, "flat": true,
	}
)

// addTypeHint explains the most common shape mistake: calling a string/array method on a plain object
// (e.g. fs.list(p, { detail: true }) items treated as strings).
func addTypeHint(msg string) string {
	m := noMemberRegex.FindStringSubmatchIndex(msg)
	if m == nil {
		return msg
	}
	name := msg[m[2]:m[3]]
	var hint string
	switch {
	case stringOnlyMethods[name]:
		hint = " (the value is an object, not a string; check the API's return shape)"
	case arrayOnlyMethods[name]:
		hint = " (the value is an object, not an array; check the API's return shape)"
	default:
		return msg
	}
	return msg[:m[1]] + hint + msg[m[1]:]
}

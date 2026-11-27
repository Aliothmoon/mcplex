// Package sandbox executes compiled TypeScript programs inside isolated goja JavaScript VMs
// with concurrent host Promise bridging, operation budgets, and deadline enforcement.
package sandbox

import (
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
	DefaultTimeout   = 20 * time.Second
	MaxTimeout       = 120 * time.Second
	DefaultMaxOps    = 400
	MaxLogEntries    = 2000
	MaxLogBytes      = 256 * 1024
	maxLogReentrancy = 4
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
	deadlineCtx   *extendableDeadlineCtx
	Cwd           string
	VM            *goja.Runtime
	loopCh        chan loopAction
	deliverFn     goja.Callable
	makeErrFn     goja.Callable
	hostObjProto  *goja.Object
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
	logDepth      int // VM-thread only: guards console.log re-entrancy via property getters
	pendingOps    atomic.Int32
	vmActiveAsync int // VM-thread only: count of unsettled async ops
	loopTurn      int // VM-thread only: increments on each loopCh turn
	asyncWaveTurn int // VM-thread only: loopTurn in which the active parallel async wave started
	waveSeq       int // VM-thread only: increments whenever a new serial step or parallel wave starts
	startedAt     time.Time
	cleanupFns    []func()
	warnMu        sync.Mutex
	warnings      []string
	warnCount     map[string]int
	compiled      *compiler.CompileResult
	asyncErr      error // VM-thread only: first exception/interrupt raised outside the main promise chain

	timersMu      sync.Mutex
	timers        map[int64]*timerEntry
	nextTimerID   int64
	timerWake     chan struct{}
	timerDone     chan struct{}
	timerOnce     sync.Once
	timerStopOnce sync.Once
	timersClosed  bool
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
			panic(env.newJSHostError(KindError, capability, opBudgetMessage(capability, env.maxOps), line, nil))
		}
		exported, argErr := safeExportArgs(env.VM, call.Arguments)
		if argErr != nil {
			rethrowIfInterrupted(argErr)
			panic(env.newJSHostError(KindBadArgs, capability, safeErrString(argErr), line, nil))
		}
		target := summarizeTarget(exported)
		if env.onOpStart != nil {
			env.onOpStart(capability, target, seq)
		}
		if env.vmActiveAsync == 0 || env.asyncWaveTurn != env.loopTurn {
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
			errVal := env.newJSHostError(KindError, capability, opBudgetMessage(capability, env.maxOps), line, nil)
			if rErr := reject(errVal); rErr != nil {
				env.setAsyncErr(rErr)
			} else if _, dErr := env.deliverFn(goja.Undefined()); dErr != nil {
				env.setAsyncErr(dErr)
			}
			return env.VM.ToValue(promise)
		}
		exported, argErr := safeExportArgs(env.VM, call.Arguments)
		if argErr != nil {
			rethrowIfInterrupted(argErr)
			promise, _, reject := env.VM.NewPromise()
			errVal := env.newJSHostError(KindBadArgs, capability, safeErrString(argErr), line, nil)
			if rErr := reject(errVal); rErr != nil {
				env.setAsyncErr(rErr)
			} else if _, dErr := env.deliverFn(goja.Undefined()); dErr != nil {
				env.setAsyncErr(dErr)
			}
			return env.VM.ToValue(promise)
		}
		target := summarizeTarget(exported)
		if env.onOpStart != nil {
			env.onOpStart(capability, target, seq)
		}

		promise, resolve, reject := env.VM.NewPromise()
		if env.vmActiveAsync == 0 || env.asyncWaveTurn != env.loopTurn {
			env.waveSeq++
			env.asyncWaveTurn = env.loopTurn
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
			res, err := func() (val any, fnErr error) {
				defer func() {
					if r := recover(); r != nil {
						fnErr = fmt.Errorf("host capability panic in %s: %v", capability, safeErrString(recoveredToError(nil, r)))
					}
				}()
				return fn(env.Ctx, exported)
			}()
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
					defer func() {
						if r := recover(); r != nil {
							env.setAsyncErr(recoveredToError(env.VM, r))
						}
					}()
					// Decrement vmActiveAsync BEFORE settling the Promise so a sequential `await`
					// resuming inside resolve/deliverFn starts a fresh wave instead of joining this one.
					if env.vmActiveAsync > 0 {
						env.vmActiveAsync--
					}
					if err != nil {
						if rErr := reject(jsErr()); rErr != nil {
							env.setAsyncErr(rErr)
							return
						}
						if _, dErr := env.deliverFn(goja.Undefined()); dErr != nil {
							env.setAsyncErr(dErr)
						}
						return
					}
					val := env.toJSValue(res)
					if rErr := resolve(val); rErr != nil {
						env.setAsyncErr(rErr)
						return
					}
					if _, dErr := env.deliverFn(goja.Undefined()); dErr != nil {
						env.setAsyncErr(dErr)
					}
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
	if gv, ok := v.(goja.Value); ok {
		return gv
	}
	if om, ok := v.(*OrderedMap); ok {
		val := env.orderedToJS(om)
		if env.hostObjProto != nil {
			if obj := val.ToObject(env.VM); obj != nil {
				_ = obj.SetPrototype(env.hostObjProto)
			}
		}
		return val
	}
	// Round-trip structs via JSON into clean JS objects/arrays so Object.keys, destructuring, camelCase JSON tags,
	// and exact runtime receiver type hints (`object {isDir, name, size}`) work seamlessly in TS/JS.
	switch x := v.(type) {
	case string, bool, int, int64, float64:
		return env.VM.ToValue(x)
	case []string:
		items := make([]any, len(x))
		for i, s := range x {
			items[i] = s
		}
		return env.VM.NewArray(items...)
	case []any:
		items := make([]any, len(x))
		for i, item := range x {
			items[i] = env.toJSValue(item)
		}
		return env.VM.NewArray(items...)
	case map[string]any:
		obj := env.VM.NewObject()
		if env.hostObjProto != nil {
			_ = obj.SetPrototype(env.hostObjProto)
		}
		for k, item := range x {
			_ = obj.Set(k, env.toJSValue(item))
		}
		return obj
	default:
		raw, err := json.Marshal(v)
		if err != nil {
			return env.VM.ToValue(v)
		}
		var generic any
		if err := json.Unmarshal(raw, &generic); err != nil {
			return env.VM.ToValue(v)
		}
		return env.toJSValue(generic)
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
	full := len(env.logs) >= MaxLogEntries || env.logBytes >= MaxLogBytes
	env.logsMu.Unlock()
	if full {
		return
	}

	// Guard against recursive console.log calls inside JS property getters / Proxy traps.
	if env.logDepth >= maxLogReentrancy {
		return
	}
	env.logDepth++
	defer func() { env.logDepth-- }()

	// Evaluate & format all arguments OUTSIDE env.logsMu.Lock() so nested console.log calls never deadlock!
	parts := make([]string, len(args))
	for i, a := range args {
		parts[i] = safeFormatLogArg(env.VM, a)
	}
	line := strings.Join(parts, " ")
	if level != "" && level != "log" {
		line = "[" + level + "] " + line
	}

	env.logsMu.Lock()
	defer env.logsMu.Unlock()
	if len(env.logs) >= MaxLogEntries || env.logBytes >= MaxLogBytes {
		return
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
	defer func() {
		if r := recover(); r != nil {
			result = nil
			retErr = &ExecutionError{
				Phase:      "runtime",
				Message:    safeErrString(recoveredToError(nil, r)),
				DurationMs: time.Since(start).Milliseconds(),
			}
		}
	}()

	cr, err := compiler.Compile(req.Code, req.Payloads)
	if err != nil {
		return nil, &ExecutionError{
			Phase:      "compile",
			Message:    err.Error(),
			DurationMs: time.Since(start).Milliseconds(),
		}
	}

	timeoutExplicit := req.Timeout > 0
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

	dlCtx, cancel := newExtendableDeadlineCtx(parentCtx, timeout, maxTimeout, timeoutExplicit)
	var ctx context.Context = dlCtx

	vm := goja.New()
	vm.SetMaxCallStackSize(MaxCallStackDepth)
	vm.SetFieldNameMapper(goja.TagFieldNameMapper("json", true))

	env := &ExecutionEnv{
		ExecID:        fmt.Sprintf("exec:%d", sandboxExecSeq.Add(1)),
		Ctx:           ctx,
		deadlineCtx:   dlCtx,
		Cwd:           req.Cwd,
		VM:            vm,
		loopCh:        make(chan loopAction, 256),
		maxOps:        maxOps,
		onOpStart:     req.OnOpStart,
		inFlight:      map[int]inFlightEntry{},
		asyncWaveTurn: -1,
		startedAt:     start,
		compiled:      cr,
	}
	// Crucial teardown order on exit:
	// 0) recover any unexpected panic into a clean runtime ExecutionError BEFORE cancel() mutates env.Ctx
	// 1) cancel() kills all child process trees via cmd.Cancel() & cleanupTimers() stops pending timers
	// 2) waitAsyncDrain(800ms) waits for subprocess handles to release
	// 3) cleanupFns roll back uncommitted tx.* file snapshots safely without Windows file-lock contention!
	defer func() {
		if r := recover(); r != nil {
			result = nil
			retErr = env.wrapRuntimeErr(recoveredToError(env.VM, r), cr, start)
		}
		cancel()
		env.cleanupTimers()
		env.waitAsyncDrain(800 * time.Millisecond)
		for i := len(env.cleanupFns) - 1; i >= 0; i-- {
			func(fn func()) {
				defer func() {
					if r := recover(); r != nil {
						env.AddWarning("cleanup panic: " + safeErrString(recoveredToError(env.VM, r)))
					}
				}()
				fn()
			}(env.cleanupFns[i])
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

	// Watchdog goroutine interrupts infinite synchronous CPU loops in JS (`while(true){}`) immediately on deadline,
	// and re-asserts vm.Interrupt every 10ms until doneWatch closes so queued callbacks never spin after ClearInterrupt().
	doneWatch := make(chan struct{})
	defer close(doneWatch)
	go func() {
		select {
		case <-ctx.Done():
			reason := "execution deadline exceeded"
			if parentCtx.Err() != nil && !errors.Is(parentCtx.Err(), context.DeadlineExceeded) {
				reason = "execution canceled"
			}
			vm.Interrupt(reason)
			ticker := time.NewTicker(10 * time.Millisecond)
			defer ticker.Stop()
			for {
				select {
				case <-doneWatch:
					return
				case <-ticker.C:
					vm.Interrupt(reason)
				}
			}
		case <-doneWatch:
		}
	}()

	deliverVal, err := vm.RunProgram(deliverProg)
	if err != nil {
		return nil, fmt.Errorf("bootstrap deliver failed: %w", err)
	}
	deliverFn, ok := goja.AssertFunction(deliverVal)
	if !ok {
		return nil, errors.New("bootstrap deliver is not callable")
	}
	env.deliverFn = deliverFn

	makeErrVal, err := vm.RunProgram(makeErrProg)
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

	if err := installBootstrapWrappers(env); err != nil {
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
			var expErr error
			exported, expErr = safeExportValue(vm, retVal)
			if expErr != nil {
				return nil, env.wrapRuntimeErr(expErr, cr, start)
			}
		}
		return &ExecResult{
			Value:      exported,
			HasValue:   hasVal,
			Logs:       env.getLogs(),
			Traces:     env.getTraces(),
			DurationMs: time.Since(start).Milliseconds(),
		}, nil
	}

	handleDeadlineOrCancel := func() error {
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
			return &ExecutionError{
				Phase:       "canceled",
				Message:     "Canceled: execution canceled by client",
				TimedOutOps: culprits,
				Logs:        env.getLogs(),
				Traces:      env.getTraces(),
				DurationMs:  time.Since(start).Milliseconds(),
			}
		}
		effTimeout := dlCtx.effectiveTimeout(timeout)
		clipSuffix := ""
		if note := dlCtx.clippedTimeoutNote(); note != "" {
			clipSuffix = "; outer timeoutMs clipped " + note
		}
		return &ExecutionError{
			Phase:       "timeout",
			Message:     fmt.Sprintf("Timeout: program exceeded %s (timeoutMs=%d, max %d%s)", effTimeout, effTimeout.Milliseconds(), maxTimeout.Milliseconds(), clipSuffix),
			TimedOutOps: culprits,
			Logs:        env.getLogs(),
			Traces:      env.getTraces(),
			DurationMs:  time.Since(start).Milliseconds(),
		}
	}

	for promise.State() == goja.PromiseStatePending {
		if env.asyncErr != nil {
			wrappedErr := env.wrapRuntimeErr(env.asyncErr, cr, start)
			cancel()
			return nil, wrappedErr
		}
		if ctx.Err() != nil {
			return nil, handleDeadlineOrCancel()
		}
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
			env.loopTurn++
			act.run()
			if env.asyncErr != nil {
				wrappedErr := env.wrapRuntimeErr(env.asyncErr, cr, start)
				cancel()
				return nil, wrappedErr
			}
		case <-ctx.Done():
			return nil, handleDeadlineOrCancel()
		}
	}

	if env.asyncErr != nil {
		wrappedErr := env.wrapRuntimeErr(env.asyncErr, cr, start)
		cancel()
		return nil, wrappedErr
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
		formattedMsg = enrichTypeHint(vm, formattedMsg, cr)
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
		var expErr error
		exported, expErr = safeExportValue(vm, res)
		if expErr != nil {
			return nil, env.wrapRuntimeErr(expErr, cr, start)
		}
	}
	return &ExecResult{
		Value:      exported,
		HasValue:   hasVal,
		Logs:       env.getLogs(),
		Traces:     env.getTraces(),
		DurationMs: time.Since(start).Milliseconds(),
	}, nil
}

// MaxCallStackDepth bounds JS call depth (roughly Node's default ~10k frames) so runaway recursion fails in
// milliseconds with bounded memory instead of running until the exec timeout. Async functions consume about
// two frames per level, so ~4k levels of async recursion still work.
const MaxCallStackDepth = 8192

// stackOverflowMessage replaces Goja's message-less StackOverflowError with an actionable RangeError text.
// Goja treats stack overflow as uncatchable, so the hint says so explicitly.
func stackOverflowMessage(err error) string {
	loc := strings.TrimSpace(safeErrString(err))
	msg := fmt.Sprintf("RangeError: Maximum call stack size exceeded (more than %d nested calls). This is not catchable by try/catch inside the sandbox; convert deep recursion to an explicit loop/stack", MaxCallStackDepth)
	if loc != "" {
		if !strings.HasPrefix(loc, "at ") {
			loc = "at " + loc
		}
		msg += " " + loc
	}
	return msg
}

func (env *ExecutionEnv) wrapRuntimeErr(err error, cr *compiler.CompileResult, start time.Time) error {
	phase := "runtime"
	var interrupted *goja.InterruptedError
	var stackOverflow *goja.StackOverflowError
	if errors.As(err, &stackOverflow) {
		formattedMsg := compiler.CleanStack(compiler.FormatRuntimeError(stackOverflowMessage(stackOverflow), cr), cr)
		return &ExecutionError{
			Phase:       phase,
			Message:     formattedMsg,
			TimedOutOps: env.snapshotInFlight(),
			Logs:        env.getLogs(),
			Traces:      env.getTraces(),
			DurationMs:  time.Since(start).Milliseconds(),
		}
	}
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
	formattedMsg := compiler.FormatRuntimeError(safeErrString(err), cr)
	if phase == "runtime" {
		formattedMsg = enrichUnknownMethodError(env.VM, formattedMsg, cr)
		formattedMsg = enrichTypeHint(env.VM, formattedMsg, cr)
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
	if _, err := env.VM.RunProgram(consoleExtrasProg); err != nil {
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
	return vm.RunProgram(payloadProxyProg)
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

func opBudgetMessage(capability string, maxOps int) string {
	base := fmt.Sprintf("op budget exceeded (%d host calls); batch work (e.g. ast.outlines, sh.checks) or split the program", maxOps)
	switch capability {
	case "fs.rm":
		return base + "; for recursive directory deletion, use built-in run_command instead of looping fs.rm"
	case "fs.stat":
		return base + "; for bulk file sizes/mtimes, use search.glob(\"**/*\", { path: dir, detail: true }) or fs.list(dir, { detail: true }) in 1 call instead of looping fs.stat"
	case "fs.read":
		return base + "; for reading many files at once, use fs.cat(paths) or search.rg(...) instead of looping fs.read"
	}
	return base
}

// installBootstrapWrappers adds ergonomic JS facades over raw host namespaces:
// 1. `mcp.<server>.<tool>(args)` dynamic Proxy routing to `mcp.call("server/tool", args)`
// 2. `tx.begin()` returning a first-class handle `{ id, edit, write, verify, commit, rollback }`
// 3. Common LLM method aliases (`fs.readFile` -> `fs.read`, `search.grep` -> `search.rg`, etc.) + clear `UnknownMethodError` listing valid methods!
func installBootstrapWrappers(env *ExecutionEnv) error {
	vm := env.VM
	_, err := vm.RunProgram(bootstrapWrappersProg)
	if err != nil {
		return err
	}
	if protoVal := vm.Get("__ptcHostObjProto"); protoVal != nil && !goja.IsUndefined(protoVal) && !goja.IsNull(protoVal) {
		env.hostObjProto = protoVal.ToObject(vm)
	}
	_ = vm.GlobalObject().Delete("__ptcHostObjProto")
	return nil
}

var (
	deliverProg = goja.MustCompile("deliver.js", `(function __flushMicrotasks() {})`, false)
	makeErrProg = goja.MustCompile("makeErr.js", `(function __makeHostError(name, op, msg, line, data) {
		const e = new Error(op + ": " + msg);
		Object.defineProperty(e, "name", { value: name, enumerable: true, writable: true, configurable: true });
		Object.defineProperty(e, "message", { value: e.message, enumerable: true, writable: true, configurable: true });
		Object.defineProperty(e, "op", { value: op, writable: true, configurable: true });
		if (data) e.data = data;
		Object.defineProperty(e, "stack", { value: name + ": " + e.message + (line > 0 ? "\n    at code:" + line : ""), writable: true, configurable: true });
		Object.defineProperty(e, "toJSON", { value: function () { return this.name + ": " + this.message; }, configurable: true });
		return e;
	})`, false)
	consoleExtrasProg     = goja.MustCompile("console_extras.js", consoleExtrasJS, false)
	payloadProxyProg      = goja.MustCompile("payload_proxy.js", payloadProxyJS, false)
	bootstrapWrappersProg = goja.MustCompile("wrappers.js", `(function() {
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
						replace: (path, target, replacement, opts) => rawTx._edit(id, path, Object.assign({}, opts || {}, { target, replacement })),
						replaceSymbol: (path, symbol, replacement) => rawTx._replaceSymbol(id, path, symbol, replacement),
						insertSymbol: (path, symbol, position, replacement) => rawTx._edit(id, path, { symbol, insert: position, replacement }),
						append: (path, content) => rawTx._edit(id, path, { insert: "eof", replacement: content }),
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
				readBytes: "bytes", readBinary: "bytes", readBuffer: "bytes", checksum: "hash", digest: "hash",
				existsSync: "exists", lstat: "stat" },
			search: { grep: "rg", find: "glob", files: "glob" },
			ast: { symbols: "outline", parse: "outline", readSymbol: "read", symbol: "read", getSymbol: "read" },
			edit: { edit: "apply", patch: "apply" },
			sh: { bash: "exec", pwsh: "exec", check: "checks", execFile: "run", spawn: "run" },
			state: { load: "get", store: "set", del: "delete", remove: "delete" },
		};

		globalThis.__ptcLastMissingMethod = null;
		globalThis.__ptcLastReceiverType = null;

		function __ptcDescribeReceiver(v) {
			if (v === null) return { kind: "null", label: "null" };
			if (v === undefined) return { kind: "undefined", label: "undefined" };
			const t = typeof v;
			if (t === "string" || v instanceof String) {
				const s = String(v);
				const prev = s.length > 40 ? s.slice(0, 40) + "..." : s;
				return { kind: "string", label: "string (len=" + s.length + ", " + JSON.stringify(prev) + ")" };
			}
			if (t === "number" || v instanceof Number) {
				return { kind: "number", label: "number (" + String(Number(v)) + ")" };
			}
			if (t === "boolean" || v instanceof Boolean) {
				return { kind: "boolean", label: "boolean (" + String(Boolean(v)) + ")" };
			}
			if (t === "bigint" || t === "symbol") {
				return { kind: t, label: t + " (" + String(v) + ")" };
			}
			if (t === "function") {
				return { kind: "function", label: "function" + (v.name ? " " + v.name : "") };
			}
			if (Array.isArray(v)) {
				if (v.length === 0) return { kind: "array", label: "Array (len=0)" };
				const first = v[0];
				let elem = first === null ? "null" : Array.isArray(first) ? "Array" : typeof first;
				if (elem === "object" && first) {
					const ks = Object.keys(first).sort();
					elem = ks.length > 0 ? "object {" + ks.slice(0, 6).join(", ") + (ks.length > 6 ? ", ..." : "") + "}" : "object {}";
				}
				return { kind: "array", label: "Array (len=" + v.length + ", item: " + elem + ")" };
			}
			if ((typeof Promise !== "undefined" && v instanceof Promise) || (t === "object" && typeof v.then === "function")) {
				return { kind: "promise", label: "Promise" };
			}
			if (typeof Map !== "undefined" && v instanceof Map) {
				return { kind: "map", label: "Map (size=" + v.size + ")" };
			}
			if (typeof Set !== "undefined" && v instanceof Set) {
				return { kind: "set", label: "Set (size=" + v.size + ")" };
			}
			if (v instanceof RegExp) {
				return { kind: "regexp", label: "RegExp (" + String(v) + ")" };
			}
			if (v instanceof Date) {
				return { kind: "date", label: "Date" };
			}
			if (typeof Uint8Array !== "undefined" && v instanceof Uint8Array) {
				return { kind: "bytes", label: "Buffer/Uint8Array (byteLength=" + v.byteLength + ")" };
			}
			if (t === "object") {
				const ks = Object.keys(v).sort();
				if (ks.length === 0) return { kind: "object", label: "object {}" };
				return { kind: "object", label: "object {" + ks.slice(0, 8).join(", ") + (ks.length > 8 ? ", ..." : "") + "}" };
			}
			return { kind: t, label: t };
		}

		const __ptcProtoTap = new Proxy(Object.prototype, {
			getPrototypeOf() { return Object.prototype; },
			has(target, prop) { return Reflect.has(target, prop); },
			get(target, prop, receiver) {
				if (typeof prop !== "string" || prop === "then" || prop === "toJSON" || prop in target) {
					return Reflect.get(target, prop, receiver);
				}
				try {
					const info = __ptcDescribeReceiver(receiver);
					globalThis.__ptcLastReceiverType = { prop, kind: info.kind, label: info.label };
				} catch (_) {}
				return undefined;
			}
		});
		for (const Ctor of [String, Number, Boolean, Array, Function, RegExp, Date, Promise, Map, Set]) {
			if (typeof Ctor === "function" && Ctor.prototype) {
				try { Object.setPrototypeOf(Ctor.prototype, __ptcProtoTap); } catch (_) {}
			}
		}
		if (typeof Uint8Array === "function" && Uint8Array.prototype) {
			const taProto = Object.getPrototypeOf(Uint8Array.prototype);
			if (taProto && taProto !== Object.prototype) {
				try { Object.setPrototypeOf(taProto, __ptcProtoTap); } catch (_) {}
			}
		}
		globalThis.__ptcHostObjProto = Object.create(__ptcProtoTap);

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
	})()`, false)
)

const consoleExtrasJS = `(function() {
	const c = globalThis.console;
	if (!c) return;
	const timers = new Map();
	const counts = new Map();
	c.dir = (obj) => c.log(obj);
	c.table = (data) => c.log(data);
	c.clear = () => {};
	c.assert = (cond, ...args) => { if (!cond) c.error("Assertion failed:", ...args); };
	c.time = (label = "default") => { timers.set(String(label), typeof performance !== "undefined" ? performance.now() : Date.now()); };
	c.timeLog = (label = "default", ...args) => {
		const k = String(label);
		const start = timers.get(k);
		if (start !== undefined) {
			const elapsed = (typeof performance !== "undefined" ? performance.now() : Date.now()) - start;
			c.log(k + ": " + elapsed.toFixed(2) + "ms", ...args);
		}
	};
	c.timeEnd = (label = "default") => {
		const k = String(label);
		const start = timers.get(k);
		if (start !== undefined) {
			timers.delete(k);
			const elapsed = (typeof performance !== "undefined" ? performance.now() : Date.now()) - start;
			c.log(k + ": " + elapsed.toFixed(2) + "ms");
		}
	};
	c.count = (label = "default") => {
		const k = String(label);
		const n = (counts.get(k) || 0) + 1;
		counts.set(k, n);
		c.log(k + ": " + n);
	};
	c.countReset = (label = "default") => { counts.delete(String(label)); };
	c.trace = (...args) => { c.log("Trace:", ...args); };
})()`

const payloadProxyJS = `(function() {
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
		},
		has(t, prop) {
			return prop in t;
		},
		ownKeys(t) {
			return Reflect.ownKeys(t);
		},
		getOwnPropertyDescriptor(t, prop) {
			return Object.getOwnPropertyDescriptor(t, prop);
		}
	});
})()`

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
	var exported any
	_ = tryJS(vm, func() {
		val := vm.Get("__ptcLastMissingMethod")
		if val != nil && !goja.IsUndefined(val) && !goja.IsNull(val) {
			exported, _ = safeExportValue(vm, val)
		}
	})
	m, ok := exported.(map[string]any)
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

func formatRejectedValue(vm *goja.Runtime, rej goja.Value) (out string) {
	defer func() {
		if r := recover(); r != nil {
			out = "Uncaught rejection (" + safeErrString(recoveredToError(vm, r)) + ")"
		}
	}()
	if rej == nil || goja.IsUndefined(rej) || goja.IsNull(rej) {
		return "Promise rejected with undefined"
	}
	var obj *goja.Object
	_ = tryJS(vm, func() {
		obj = rej.ToObject(vm)
	})
	if obj != nil {
		var msgStr, stackStr string
		var getterErr error
		if err := tryJS(vm, func() {
			if m := obj.Get("message"); m != nil && !goja.IsUndefined(m) && !goja.IsNull(m) {
				msgStr = strings.TrimSpace(m.String())
			}
		}); err != nil {
			getterErr = err
		}
		if err := tryJS(vm, func() {
			if s := obj.Get("stack"); s != nil && !goja.IsUndefined(s) && !goja.IsNull(s) {
				stackStr = strings.TrimSpace(s.String())
			}
		}); err != nil && getterErr == nil {
			getterErr = err
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
		if exp, expErr := safeExportValue(vm, rej); expErr == nil {
			if b, err := json.Marshal(exp); err == nil && string(b) != "{}" && string(b) != "null" {
				return string(b)
			}
		} else if getterErr == nil {
			getterErr = expErr
		}
		if getterErr != nil {
			return "Thrown object property getter failed: " + safeErrString(getterErr)
		}
	}
	var s string
	if err := tryJS(vm, func() { s = rej.String() }); err != nil {
		return "Thrown value toString failed: " + safeErrString(err)
	}
	if s == "[object Object]" {
		if exp, expErr := safeExportValue(vm, rej); expErr == nil {
			if b, err := json.Marshal(exp); err == nil {
				return string(b)
			}
		}
	}
	return s
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

// enrichTypeHint reports the exact runtime type (and keys/preview) of the receiver when a non-existent
// method is invoked (e.g. calling `.map()` on a `string` from `git.log()`, `.endsWith()` on an `{isDir, name, size}`
// object from `fs.list({ detail: true })`, `.split()` on an `Array`, or `.filter()` on an un-awaited `Promise`).
func enrichTypeHint(vm *goja.Runtime, formattedMsg string, cr *compiler.CompileResult) string {
	if !strings.Contains(formattedMsg, "TypeError:") || strings.Contains(formattedMsg, "UnknownMethodError:") || strings.Contains(formattedMsg, "Cannot read property ") {
		return formattedMsg
	}
	if strings.Contains(formattedMsg, "TypeError: object is not iterable") && !strings.Contains(formattedMsg, "Object.values") {
		return strings.Replace(
			formattedMsg,
			"TypeError: object is not iterable",
			"TypeError: object is not iterable (plain objects are not iterable with for..of — use Object.values(obj) or Object.entries(obj), or check Array.isArray(x))",
			1,
		)
	}

	if vm != nil {
		var exported any
		_ = tryJS(vm, func() {
			val := vm.Get("__ptcLastReceiverType")
			if val != nil && !goja.IsUndefined(val) && !goja.IsNull(val) {
				exported, _ = safeExportValue(vm, val)
			}
		})
		if m, ok := exported.(map[string]any); ok {
			prop, _ := m["prop"].(string)
			kind, _ := m["kind"].(string)
			label, _ := m["label"].(string)
			if prop != "" && label != "" && matchesReceiverErrorForProp(formattedMsg, prop, cr) {
				detail := formatReceiverTypeDetail(prop, kind, label)
				header := fmt.Sprintf("TypeError: Object has no member '%s' (%s)", prop, detail)
				if nl := strings.IndexByte(formattedMsg, '\n'); nl >= 0 {
					return header + formattedMsg[nl:]
				}
				return header
			}
		}
	}

	m := noMemberRegex.FindStringSubmatchIndex(formattedMsg)
	if m == nil {
		return formattedMsg
	}
	name := formattedMsg[m[2]:m[3]]
	detail := formatReceiverTypeDetail(name, "plain_object", "plain object")
	hint := " (" + detail + ")"
	return formattedMsg[:m[1]] + hint + formattedMsg[m[1]:]
}

func matchesReceiverErrorForProp(formattedMsg, prop string, cr *compiler.CompileResult) bool {
	matchedErr := false
	if sub := gojaNoMemberRegex.FindStringSubmatch(formattedMsg); len(sub) >= 2 {
		matchedErr = sub[1] == prop
	} else if sub := gojaNotFuncRegex.FindStringSubmatch(formattedMsg); len(sub) >= 1 {
		if sub[1] != "" {
			matchedErr = sub[1] == prop
		} else if sub[2] != "" {
			matchedErr = sub[2] == prop || strings.HasSuffix(sub[2], "."+prop)
		} else {
			matchedErr = true
		}
	}
	if !matchedErr {
		return false
	}
	if cr != nil && len(cr.SourceLine) > 0 {
		if loc := codeStackLineRegex.FindStringSubmatch(formattedMsg); len(loc) >= 2 {
			if lineNo, err := strconv.Atoi(loc[1]); err == nil && lineNo >= 1 && lineNo <= len(cr.SourceLine) {
				for idx := max(0, lineNo-2); idx <= min(len(cr.SourceLine)-1, lineNo); idx++ {
					line := cr.SourceLine[idx]
					if strings.Contains(line, "."+prop) || strings.Contains(line, `"`+prop+`"`) || strings.Contains(line, `'`+prop+`'`) {
						return true
					}
				}
				return false
			}
		}
	}
	return true
}

func formatReceiverTypeDetail(prop, kind, label string) string {
	if kind == "promise" {
		return fmt.Sprintf("actual type: %s — did you forget 'await'?", label)
	}
	switch {
	case stringOnlyMethods[prop]:
		return fmt.Sprintf("actual type: %s, expected string; check the API's return shape", label)
	case arrayOnlyMethods[prop]:
		if kind == "map" || kind == "set" {
			return fmt.Sprintf("actual type: %s, expected Array; convert with Array.from(...) first", label)
		}
		return fmt.Sprintf("actual type: %s, expected Array; check the API's return shape", label)
	default:
		return fmt.Sprintf("actual type: %s", label)
	}
}

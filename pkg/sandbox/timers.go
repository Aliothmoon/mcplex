package sandbox

import (
	"fmt"
	"sort"
	"time"

	"github.com/dop251/goja"
)

const maxActiveTimers = 1024

type timerEntry struct {
	id         int64
	due        time.Time
	interval   time.Duration
	repeat     bool
	fn         goja.Callable
	extra      []goja.Value
	dispatched bool
	canceled   bool
	settled    bool
}

// OrderedMap is returned by host functions that must preserve key order in the guest object
// (Go maps iterate randomly). Keys and Vals are parallel slices.
type OrderedMap struct {
	Keys []string
	Vals []any
}

// installTimers adds the standard `setTimeout` / `clearTimeout` and `structuredClone` globals models reach for by
// habit (`await new Promise(r => setTimeout(r, 500))`).
// Active timers are capped at 1024, dispatched via a single deterministic coordinator so same-delay timers
// strictly preserve FIFO registration order, and cleaned up immediately when `Execute` exits.
var timersCloneProg = goja.MustCompile("timers_clone.js", `(function () {
	function clone(v, seen) {
		if (v === null || typeof v !== "object") return v;
		if (seen.has(v)) return seen.get(v);
		if (v instanceof Date) return new Date(v.getTime());
		if (v instanceof RegExp) return new RegExp(v.source, v.flags);
		if (typeof Buffer === "function" && v instanceof Buffer) return Buffer.from(v);
		if (ArrayBuffer.isView(v)) return v.slice();
		if (v instanceof ArrayBuffer) return v.slice(0);
		if (v instanceof Map) { const m = new Map(); seen.set(v, m); v.forEach((x, k) => m.set(clone(k, seen), clone(x, seen))); return m; }
		if (v instanceof Set) { const s = new Set(); seen.set(v, s); v.forEach((x) => s.add(clone(x, seen))); return s; }
		const out = Array.isArray(v) ? [] : {};
		seen.set(v, out);
		for (const k of Object.keys(v)) out[k] = clone(v[k], seen);
		return out;
	}
	globalThis.structuredClone = function structuredClone(v) { return clone(v, new Map()); };
	globalThis.queueMicrotask = function queueMicrotask(cb) {
		if (typeof cb !== "function") throw new TypeError("queueMicrotask: callback must be a function");
		Promise.resolve().then(() => cb());
	};
})()`, false)

func installTimers(env *ExecutionEnv) error {
	vm := env.VM
	maxDelay := EffectiveMaxTimeout()
	rangeErrCtor, _ := goja.AssertConstructor(vm.Get("RangeError"))

	registerTimer := func(opName string, call goja.FunctionCall, hasDelay bool, repeat bool) goja.Value {
		fn, ok := goja.AssertFunction(call.Argument(0))
		if !ok {
			panic(vm.NewTypeError(opName + ": callback must be a function"))
		}
		var delay time.Duration
		argStart := 1
		if hasDelay {
			delay = time.Duration(call.Argument(1).ToInteger()) * time.Millisecond
			argStart = 2
		}
		if delay < 0 {
			delay = 0
		}
		if repeat && delay < time.Millisecond {
			delay = time.Millisecond
		}
		if delay > maxDelay {
			delay = maxDelay
		}
		var extra []goja.Value
		if len(call.Arguments) > argStart {
			extra = append(extra, call.Arguments[argStart:]...)
		}

		env.startTimerCoordinator()

		env.timersMu.Lock()
		if env.timersClosed {
			env.timersMu.Unlock()
			return vm.ToValue(int64(0))
		}
		if len(env.timers) >= maxActiveTimers {
			env.timersMu.Unlock()
			msg := fmt.Sprintf("%s: active timer limit (%d) exceeded", opName, maxActiveTimers)
			if rangeErrCtor != nil {
				if errObj, err := rangeErrCtor(nil, vm.ToValue(msg)); err == nil {
					panic(errObj)
				}
			}
			panic(vm.NewGoError(fmt.Errorf("RangeError: %s", msg)))
		}
		env.nextTimerID++
		id := env.nextTimerID
		entry := &timerEntry{
			id:       id,
			due:      time.Now().Add(delay),
			interval: delay,
			repeat:   repeat,
			fn:       fn,
			extra:    extra,
		}
		env.timers[id] = entry
		env.pendingOps.Add(1)
		env.timersMu.Unlock()

		env.signalTimerWake()
		return vm.ToValue(id)
	}

	clearTimer := func(call goja.FunctionCall) goja.Value {
		id := call.Argument(0).ToInteger()
		env.timersMu.Lock()
		if entry, ok := env.timers[id]; ok {
			entry.canceled = true
			if !entry.settled {
				entry.settled = true
				env.pendingOps.Add(-1)
			}
			delete(env.timers, id)
		}
		env.timersMu.Unlock()
		return goja.Undefined()
	}

	if err := vm.Set("setTimeout", func(call goja.FunctionCall) goja.Value {
		return registerTimer("setTimeout", call, true, false)
	}); err != nil {
		return err
	}
	if err := vm.Set("clearTimeout", clearTimer); err != nil {
		return err
	}
	if err := vm.Set("setInterval", func(call goja.FunctionCall) goja.Value {
		return registerTimer("setInterval", call, true, true)
	}); err != nil {
		return err
	}
	if err := vm.Set("clearInterval", clearTimer); err != nil {
		return err
	}
	if err := vm.Set("setImmediate", func(call goja.FunctionCall) goja.Value {
		return registerTimer("setImmediate", call, false, false)
	}); err != nil {
		return err
	}
	if err := vm.Set("clearImmediate", clearTimer); err != nil {
		return err
	}

	_, err := vm.RunProgram(timersCloneProg)
	return err
}

func (env *ExecutionEnv) startTimerCoordinator() {
	env.timerOnce.Do(func() {
		env.timersMu.Lock()
		if env.timers == nil {
			env.timers = make(map[int64]*timerEntry)
		}
		env.timerWake = make(chan struct{}, 1)
		env.timerDone = make(chan struct{})
		env.timersMu.Unlock()

		go env.runTimerCoordinator()
	})
}

func (env *ExecutionEnv) signalTimerWake() {
	if env.timerWake == nil {
		return
	}
	select {
	case env.timerWake <- struct{}{}:
	default:
	}
}

func (env *ExecutionEnv) settleTimerEntry(e *timerEntry) bool {
	env.timersMu.Lock()
	defer env.timersMu.Unlock()
	canceled := e.canceled
	if !e.settled {
		e.settled = true
		env.pendingOps.Add(-1)
	}
	delete(env.timers, e.id)
	return canceled
}

func (env *ExecutionEnv) isTimerCanceled(e *timerEntry) bool {
	env.timersMu.Lock()
	defer env.timersMu.Unlock()
	return e.canceled || env.timersClosed
}

func (env *ExecutionEnv) rearmIntervalEntry(e *timerEntry) {
	env.timersMu.Lock()
	if e.canceled || env.timersClosed {
		env.timersMu.Unlock()
		return
	}
	e.due = time.Now().Add(e.interval)
	e.dispatched = false
	env.timersMu.Unlock()
	env.signalTimerWake()
}

func (env *ExecutionEnv) runTimerCoordinator() {
	var timer *time.Timer
	stopTimer := func() {
		if timer != nil {
			if !timer.Stop() {
				select {
				case <-timer.C:
				default:
				}
			}
		}
	}
	defer stopTimer()

	for {
		now := time.Now()
		var ready []*timerEntry
		var nextDue time.Time

		env.timersMu.Lock()
		if env.timersClosed {
			env.timersMu.Unlock()
			return
		}
		for _, e := range env.timers {
			if e.canceled || e.dispatched {
				continue
			}
			if !now.Before(e.due) {
				ready = append(ready, e)
			} else if nextDue.IsZero() || e.due.Before(nextDue) {
				nextDue = e.due
			}
		}
		if len(ready) > 0 {
			sort.Slice(ready, func(i, j int) bool {
				if !ready[i].due.Equal(ready[j].due) {
					return ready[i].due.Before(ready[j].due)
				}
				return ready[i].id < ready[j].id
			})
			// Dispatch the earliest expired timer first, then re-evaluate under lock so newly added/cleared timers stay ordered.
			picked := ready[0]
			picked.dispatched = true
			env.timersMu.Unlock()

			act := loopAction{
				run: func() {
					defer func() {
						if r := recover(); r != nil {
							env.setAsyncErr(recoveredToError(env.VM, r))
						}
					}()
					if picked.repeat {
						if env.isTimerCanceled(picked) {
							return
						}
					} else if env.settleTimerEntry(picked) {
						return
					}
					if _, err := picked.fn(goja.Undefined(), picked.extra...); err != nil {
						if picked.repeat {
							env.settleTimerEntry(picked)
						}
						env.setAsyncErr(err)
						return
					}
					if picked.repeat {
						env.rearmIntervalEntry(picked)
					}
					if _, dErr := env.deliverFn(goja.Undefined()); dErr != nil {
						env.setAsyncErr(dErr)
					}
				},
			}

			select {
			case env.loopCh <- act:
			case <-env.timerDone:
				env.settleTimerEntry(picked)
				return
			case <-env.Ctx.Done():
				env.settleTimerEntry(picked)
				return
			}
			continue
		}
		env.timersMu.Unlock()

		var timerCh <-chan time.Time
		if !nextDue.IsZero() {
			wait := time.Until(nextDue)
			if wait < 0 {
				wait = 0
			}
			if timer == nil {
				timer = time.NewTimer(wait)
			} else {
				stopTimer()
				timer.Reset(wait)
			}
			timerCh = timer.C
		} else {
			stopTimer()
		}

		select {
		case <-env.timerDone:
			return
		case <-env.Ctx.Done():
			return
		case <-env.timerWake:
		case <-timerCh:
		}
	}
}

func (env *ExecutionEnv) cleanupTimers() {
	env.timersMu.Lock()
	env.timersClosed = true
	if env.timerDone != nil {
		env.timerStopOnce.Do(func() { close(env.timerDone) })
	}
	for id, e := range env.timers {
		e.canceled = true
		if !e.settled {
			e.settled = true
			env.pendingOps.Add(-1)
		}
		delete(env.timers, id)
	}
	env.timersMu.Unlock()
}

// setAsyncErr records the first error raised outside the main promise chain (e.g. inside a timer callback).
func (env *ExecutionEnv) setAsyncErr(err error) {
	if env.asyncErr == nil {
		env.asyncErr = err
	}
}

// orderedToJS builds a guest object preserving OrderedMap key order.
func (env *ExecutionEnv) orderedToJS(m *OrderedMap) goja.Value {
	obj := env.VM.NewObject()
	for i, k := range m.Keys {
		_ = obj.Set(k, env.toJSValue(m.Vals[i]))
	}
	return obj
}

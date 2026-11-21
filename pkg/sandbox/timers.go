package sandbox

import (
	"time"

	"github.com/dop251/goja"
)

// OrderedMap is returned by host functions that must preserve key order in the guest object
// (Go maps iterate randomly). Keys and Vals are parallel slices.
type OrderedMap struct {
	Keys []string
	Vals []any
}

// installTimers adds the standard `setTimeout` / `clearTimeout` and `structuredClone` globals models reach for by
// habit (`await new Promise(r => setTimeout(r, 500))`). Pending timers keep the program alive like Node; an exception
// thrown inside a timer callback fails the run (Node would crash on it too).
func installTimers(env *ExecutionEnv) error {
	vm := env.VM
	timers := map[int64]*time.Timer{} // VM-thread only
	var nextID int64

	maxDelay := EffectiveMaxTimeout()
	setTimeout := func(call goja.FunctionCall) goja.Value {
		fn, ok := goja.AssertFunction(call.Argument(0))
		if !ok {
			panic(vm.NewTypeError("setTimeout: callback must be a function"))
		}
		delay := time.Duration(call.Argument(1).ToInteger()) * time.Millisecond
		if delay < 0 {
			delay = 0
		}
		if delay > maxDelay {
			delay = maxDelay
		}
		var extra []goja.Value
		if len(call.Arguments) > 2 {
			extra = append(extra, call.Arguments[2:]...)
		}
		nextID++
		id := nextID
		env.pendingOps.Add(1)
		timers[id] = time.AfterFunc(delay, func() {
			select {
			case env.loopCh <- loopAction{run: func() {
				defer env.pendingOps.Add(-1)
				if _, live := timers[id]; !live {
					return // cleared after firing
				}
				delete(timers, id)
				if _, err := fn(goja.Undefined(), extra...); err != nil {
					env.setAsyncErr(err)
				}
				_, _ = env.deliverFn(goja.Undefined())
			}}:
			case <-env.Ctx.Done():
				env.pendingOps.Add(-1)
			}
		})
		return vm.ToValue(id)
	}
	clearTimeout := func(call goja.FunctionCall) goja.Value {
		id := call.Argument(0).ToInteger()
		if t, ok := timers[id]; ok {
			if t.Stop() {
				env.pendingOps.Add(-1) // callback will never be queued
			}
			delete(timers, id)
		}
		return goja.Undefined()
	}
	if err := vm.Set("setTimeout", setTimeout); err != nil {
		return err
	}
	if err := vm.Set("clearTimeout", clearTimeout); err != nil {
		return err
	}

	_, err := vm.RunString(`(function () {
		function clone(v, seen) {
			if (v === null || typeof v !== "object") return v;
			if (seen.has(v)) return seen.get(v);
			if (v instanceof Date) return new Date(v.getTime());
			if (v instanceof RegExp) return new RegExp(v.source, v.flags);
			if (ArrayBuffer.isView(v)) return v.slice();
			if (v instanceof Map) { const m = new Map(); seen.set(v, m); v.forEach((x, k) => m.set(clone(k, seen), clone(x, seen))); return m; }
			if (v instanceof Set) { const s = new Set(); seen.set(v, s); v.forEach((x) => s.add(clone(x, seen))); return s; }
			const out = Array.isArray(v) ? [] : {};
			seen.set(v, out);
			for (const k of Object.keys(v)) out[k] = clone(v[k], seen);
			return out;
		}
		globalThis.structuredClone = function structuredClone(v) { return clone(v, new Map()); };
	})()`)
	return err
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

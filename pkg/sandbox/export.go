package sandbox

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"reflect"
	"strconv"
	"strings"
	"time"

	"github.com/aliothmoon/mcplex/pkg/output"
	"github.com/dop251/goja"
)

const (
	maxExportDepth      = 1000
	maxExportArrayLen   = 1_000_000
	maxExportTotalNodes = 2_000_000
	maxExportByteSlice  = 16 * 1024 * 1024
)

var (
	gojaMapExportType = reflect.TypeOf([][2]any{})
	gojaSetExportType = reflect.TypeOf([]any{})
)

type exportState struct {
	vm      *goja.Runtime
	visited map[*goja.Object]bool
	nodes   int
	forLog  bool
}

// tryJS executes fn — which may re-enter JS through property getters, Proxy traps or toString/valueOf —
// under vm.Try so Goja unwinds its value stack, call stack and try frames back to the entry state before the
// exception is turned into a Go error. A bare Go recover() here would leave the stack pointer of the JS frames
// pushed by the throwing callee in place, so a later `catch (e)` in the SAME program would bind the wrong value.
//
// Uncatchable exceptions (InterruptedError, StackOverflowError) are re-panicked by vm.Try only AFTER the stacks
// have been restored; the outer recover converts them so callers can decide to rethrow (see rethrowIfInterrupted).
func tryJS(vm *goja.Runtime, fn func()) (err error) {
	defer func() {
		if r := recover(); r != nil {
			err = recoveredToError(vm, r)
		}
	}()
	if vm == nil {
		fn()
		return nil
	}
	if ex := vm.Try(fn); ex != nil {
		return stableJSError(vm, ex)
	}
	return nil
}

// jsThrowError is a JS exception whose message was rendered once under vm.Try, so calling Error() later never
// re-enters JS (goja's (*Exception).Error() invokes the thrown value's toString, which may itself throw).
type jsThrowError struct {
	msg string
	ex  *goja.Exception
}

func (e *jsThrowError) Error() string { return e.msg }

// Value returns the original thrown JS value.
func (e *jsThrowError) Value() goja.Value {
	if e.ex == nil {
		return nil
	}
	return e.ex.Value()
}

func stableJSError(vm *goja.Runtime, ex *goja.Exception) error {
	if ex == nil {
		return nil
	}
	var msg string
	if vm != nil {
		if inner := vm.Try(func() { msg = ex.Error() }); inner != nil {
			msg = describeThrownWithoutJS(ex.Value()) + " (toString threw)"
		}
	} else {
		msg = describeThrownWithoutJS(ex.Value())
	}
	if strings.TrimSpace(msg) == "" {
		msg = "uncaught JS exception"
	}
	return &jsThrowError{msg: msg, ex: ex}
}

// describeThrownWithoutJS renders a thrown value without invoking any user-defined JS code.
func describeThrownWithoutJS(v goja.Value) string {
	if v == nil || goja.IsUndefined(v) {
		return "undefined"
	}
	if goja.IsNull(v) {
		return "null"
	}
	if obj, ok := v.(*goja.Object); ok {
		return "[object " + obj.ClassName() + "]"
	}
	return v.String()
}

func recoveredToError(vm *goja.Runtime, r any) error {
	if r == nil {
		return nil
	}
	if ex, ok := r.(*goja.Exception); ok {
		return stableJSError(vm, ex)
	}
	if err, ok := r.(error); ok {
		return err
	}
	if gv, ok := r.(goja.Value); ok {
		var s string
		if vm != nil {
			if ex := vm.Try(func() { s = gv.String() }); ex == nil && s != "" {
				return errors.New(s)
			}
		}
		return errors.New(describeThrownWithoutJS(gv))
	}
	return fmt.Errorf("%v", r)
}

func safeErrString(err error) (s string) {
	if err == nil {
		return ""
	}
	defer func() {
		if r := recover(); r != nil {
			s = fmt.Sprintf("error formatting exception (%T)", err)
		}
	}()
	return err.Error()
}

// safeExportValue converts a guest Goja value into a cycle-free, depth-bounded, JSON-safe Go value:
//   - Ancestor-path cycle tracking (`[Circular]`) so shared DAG references export normally without false positives
//   - Max depth 1000 (`[MaxDepth]` in logs; a RangeError for results and host-call arguments, never silent truncation)
//   - Sparse/huge Array OOM guard (`length > 1_000_000` or > 2M total nodes returns a `RangeError` before allocating)
//   - `NaN` / `±Infinity` converted to `nil` (`null` in JSON, matching `JSON.stringify`)
//   - `Proxy` (`process.env`, `P`/`payloads`) and `DynamicObject` exported via own keys + property gets
//   - `ArrayBuffer` / `Uint8Array` / `Buffer` cloned with `bytes.Clone`
//   - Throwing getters recovered into a clean `error` instead of panicking `Execute`
func safeExportValue(vm *goja.Runtime, val goja.Value) (any, error) {
	st := &exportState{
		vm:      vm,
		visited: make(map[*goja.Object]bool, 16),
		forLog:  false,
	}
	return st.exportVal(val, 0)
}

// safeExportArgs exports host capability call arguments safely, cloning any `[]byte` views so async host
// goroutines never race with guest JS mutating the underlying `ArrayBuffer` / `TypedArray` / `Buffer`.
func safeExportArgs(vm *goja.Runtime, vals []goja.Value) ([]any, error) {
	if len(vals) == 0 {
		return []any{}, nil
	}
	out := make([]any, len(vals))
	for i, v := range vals {
		if v == nil || goja.IsUndefined(v) || goja.IsNull(v) {
			out[i] = nil
			continue
		}
		exp, err := safeExportValue(vm, v)
		if err != nil {
			return nil, err
		}
		out[i] = cloneBytesDeep(exp)
	}
	return out, nil
}

func cloneBytesDeep(v any) any {
	switch x := v.(type) {
	case []byte:
		return bytes.Clone(x)
	case goja.ArrayBuffer:
		return bytes.Clone(x.Bytes())
	case []any:
		for i, item := range x {
			x[i] = cloneBytesDeep(item)
		}
		return x
	case map[string]any:
		for k, item := range x {
			x[k] = cloneBytesDeep(item)
		}
		return x
	default:
		return v
	}
}

// safeFormatLogArg formats a single `print` / `console.*` argument safely without raw `Export()`:
//   - `undefined` -> `"undefined"`, `null` -> `"null"`
//   - `NaN`, `Infinity`, `-Infinity` -> `"NaN"`, `"Infinity"`, `"-Infinity"`
//   - Plain strings -> raw string; `Error` / `RegExp` -> `.String()`
//   - Objects / Arrays / Proxies (`process.env`, `P`) -> bounded cycle-safe JSON
func safeFormatLogArg(vm *goja.Runtime, val goja.Value) string {
	if val == nil || goja.IsUndefined(val) {
		return "undefined"
	}
	if goja.IsNull(val) {
		return "null"
	}
	if goja.IsNaN(val) {
		return "NaN"
	}
	if goja.IsInfinity(val) {
		if val.ToFloat() < 0 {
			return "-Infinity"
		}
		return "Infinity"
	}

	obj, isObj := val.(*goja.Object)
	if !isObj {
		et := val.ExportType()
		if et != nil && et.Kind() == reflect.String {
			return val.String()
		}
		exp := val.Export()
		switch x := exp.(type) {
		case string:
			return x
		case bool:
			if x {
				return "true"
			}
			return "false"
		case int64:
			return strconv.FormatInt(x, 10)
		case float64:
			if math.IsNaN(x) {
				return "NaN"
			}
			if math.IsInf(x, 1) {
				return "Infinity"
			}
			if math.IsInf(x, -1) {
				return "-Infinity"
			}
			return strconv.FormatFloat(x, 'g', -1, 64)
		default:
			var s string
			if err := tryJS(vm, func() { s = val.String() }); err != nil {
				rethrowIfInterrupted(err)
				return "[Thrown: " + output.SliceUTF8Bytes(safeErrString(err), 120) + "]"
			}
			return s
		}
	}

	var className string
	if err := tryJS(vm, func() { className = obj.ClassName() }); err != nil {
		rethrowIfInterrupted(err)
		return "[Thrown: " + output.SliceUTF8Bytes(safeErrString(err), 120) + "]"
	}
	if _, isFn := goja.AssertFunction(obj); isFn {
		return "[Function]"
	}
	if className == "Error" || className == "RegExp" {
		var s string
		if err := tryJS(vm, func() { s = obj.String() }); err != nil {
			rethrowIfInterrupted(err)
			return "[" + className + ": " + output.SliceUTF8Bytes(safeErrString(err), 120) + "]"
		}
		return s
	}

	st := &exportState{
		vm:      vm,
		visited: make(map[*goja.Object]bool, 16),
		forLog:  true,
	}
	exported, err := st.exportVal(val, 0)
	if err != nil {
		rethrowIfInterrupted(err)
		return "[" + output.SliceUTF8Bytes(safeErrString(err), 160) + "]"
	}
	if s, ok := exported.(string); ok && (className == "String" || className == "Date") {
		return s
	}

	var buf bytes.Buffer
	enc := json.NewEncoder(&buf)
	enc.SetEscapeHTML(false)
	if err := enc.Encode(exported); err == nil {
		return strings.TrimRight(buf.String(), "\n")
	}
	return output.MarshalResultForBudget(exported, 4096)
}

func rethrowIfInterrupted(err error) {
	var interrupted *goja.InterruptedError
	if errors.As(err, &interrupted) {
		panic(interrupted)
	}
	var stackOverflow *goja.StackOverflowError
	if errors.As(err, &stackOverflow) {
		panic(stackOverflow)
	}
}

func (st *exportState) exportVal(val goja.Value, depth int) (any, error) {
	if val == nil || goja.IsUndefined(val) || goja.IsNull(val) {
		return nil, nil
	}
	if goja.IsNaN(val) {
		if st.forLog {
			return "NaN", nil
		}
		return nil, nil
	}
	if goja.IsInfinity(val) {
		if st.forLog {
			if val.ToFloat() < 0 {
				return "-Infinity", nil
			}
			return "Infinity", nil
		}
		return nil, nil
	}

	obj, isObj := val.(*goja.Object)
	if !isObj {
		var exp any
		if err := tryJS(st.vm, func() { exp = val.Export() }); err != nil {
			return nil, err
		}
		switch x := exp.(type) {
		case float64:
			if math.IsNaN(x) {
				if st.forLog {
					return "NaN", nil
				}
				return nil, nil
			}
			if math.IsInf(x, 0) {
				if st.forLog {
					if x < 0 {
						return "-Infinity", nil
					}
					return "Infinity", nil
				}
				return nil, nil
			}
			return x, nil
		case float32:
			f := float64(x)
			if math.IsNaN(f) || math.IsInf(f, 0) {
				if st.forLog {
					return fmt.Sprintf("%v", f), nil
				}
				return nil, nil
			}
			return f, nil
		case *goja.Symbol:
			if st.forLog {
				return val.String(), nil
			}
			return nil, nil
		default:
			return exp, nil
		}
	}

	if _, isFn := goja.AssertFunction(obj); isFn {
		if st.forLog {
			return "[Function]", nil
		}
		return nil, nil
	}

	if st.visited[obj] {
		return "[Circular]", nil
	}
	if depth >= maxExportDepth {
		if st.forLog {
			return "[MaxDepth]", nil
		}
		// Never silently truncate data handed to host calls (state.set, fs.write JSON) or returned as the result.
		return nil, fmt.Errorf("RangeError: value is nested deeper than %d levels; flatten it (e.g. store a linked list as an array) before passing or returning it", maxExportDepth)
	}
	st.nodes++
	if st.nodes > maxExportTotalNodes {
		if st.forLog {
			return "[MaxItems]", nil
		}
		return nil, fmt.Errorf("RangeError: exported value exceeds max %d elements; return a summary or write the data to a file with fs.write", maxExportTotalNodes)
	}

	st.visited[obj] = true
	defer delete(st.visited, obj)

	var className string
	if err := tryJS(st.vm, func() {
		className = obj.ClassName()
		if className == "Object" {
			// Goja reports class "Object" for Map/Set; identify them by their native export type instead.
			switch obj.ExportType() {
			case gojaMapExportType:
				className = "Map"
			case gojaSetExportType:
				className = "Set"
			}
		}
	}); err != nil {
		return nil, err
	}

	switch className {
	case "Map", "Set":
		// Map -> [[key, value], ...], Set -> [value, ...] (insertion order), matching goja's historical Export()
		// shape. Iterate through Array.from under vm.Try so user-defined iterators/getters cannot corrupt the VM.
		var listVal goja.Value
		if err := tryJS(st.vm, func() {
			arrayCtor := st.vm.Get("Array")
			if arrayCtor == nil {
				return
			}
			from, ok := goja.AssertFunction(arrayCtor.ToObject(st.vm).Get("from"))
			if !ok {
				return
			}
			v, callErr := from(arrayCtor, obj)
			if callErr != nil {
				panic(callErr)
			}
			listVal = v
		}); err != nil {
			return nil, err
		}
		if listVal == nil {
			return []any{}, nil
		}
		return st.exportVal(listVal, depth)

	case "Boolean":
		var b bool
		if err := tryJS(st.vm, func() { b = obj.ToBoolean() }); err != nil {
			return nil, err
		}
		return b, nil

	case "Number":
		var f float64
		if err := tryJS(st.vm, func() { f = obj.ToFloat() }); err != nil {
			return nil, err
		}
		if math.IsNaN(f) {
			if st.forLog {
				return "NaN", nil
			}
			return nil, nil
		}
		if math.IsInf(f, 0) {
			if st.forLog {
				if f < 0 {
					return "-Infinity", nil
				}
				return "Infinity", nil
			}
			return nil, nil
		}
		return f, nil

	case "String":
		var s string
		if err := tryJS(st.vm, func() { s = obj.String() }); err != nil {
			return nil, err
		}
		return s, nil

	case "Date":
		var iso any
		if err := tryJS(st.vm, func() {
			if toJSONVal := obj.Get("toJSON"); toJSONVal != nil {
				if fn, ok := goja.AssertFunction(toJSONVal); ok {
					res, fnErr := fn(obj)
					if fnErr == nil && res != nil && !goja.IsUndefined(res) && !goja.IsNull(res) {
						iso = res.String()
						return
					}
				}
			}
			if tm, ok := obj.Export().(time.Time); ok {
				iso = tm.UTC().Format(time.RFC3339Nano)
			}
		}); err != nil {
			return nil, err
		}
		if iso == nil && st.forLog {
			return "Invalid Date", nil
		}
		return iso, nil

	case "RegExp":
		if st.forLog {
			var s string
			if err := tryJS(st.vm, func() { s = obj.String() }); err != nil {
				return nil, err
			}
			return s, nil
		}
		return map[string]any{}, nil

	case "Array":
		var arrLen int64
		if err := tryJS(st.vm, func() {
			lenVal := obj.Get("length")
			if lenVal != nil && !goja.IsUndefined(lenVal) && !goja.IsNull(lenVal) {
				arrLen = lenVal.ToInteger()
			}
		}); err != nil {
			return nil, err
		}
		if arrLen < 0 || arrLen > maxExportArrayLen {
			if st.forLog {
				return fmt.Sprintf("[Array(%d) exceeds max %d]", arrLen, maxExportArrayLen), nil
			}
			return nil, fmt.Errorf("RangeError: exported array length %d exceeds max %d", arrLen, maxExportArrayLen)
		}
		if st.nodes+int(arrLen) > maxExportTotalNodes {
			if st.forLog {
				return fmt.Sprintf("[Array(%d) exceeds max %d total elements]", arrLen, maxExportTotalNodes), nil
			}
			return nil, fmt.Errorf("RangeError: exported array exceeds max %d total elements", maxExportTotalNodes)
		}
		out := make([]any, arrLen)
		for i := int64(0); i < arrLen; i++ {
			var elem goja.Value
			if err := tryJS(st.vm, func() { elem = obj.Get(strconv.FormatInt(i, 10)) }); err != nil {
				if st.forLog {
					rethrowIfInterrupted(err)
					out[i] = "[GetterError: " + output.SliceUTF8Bytes(safeErrString(err), 120) + "]"
					continue
				}
				return nil, err
			}
			v, err := st.exportVal(elem, depth+1)
			if err != nil {
				if st.forLog {
					rethrowIfInterrupted(err)
					out[i] = "[GetterError: " + output.SliceUTF8Bytes(safeErrString(err), 120) + "]"
					continue
				}
				return nil, err
			}
			out[i] = v
		}
		return out, nil
	}

	// Check raw ArrayBuffer or Uint8Array / Buffer byte views and clone backing slices!
	if b, isByteView, err := st.tryExtractClonedBytes(obj); err != nil {
		return nil, err
	} else if isByteView {
		return b, nil
	}

	// Non-byte TypedArrays (Int32Array, Float64Array, etc.)
	if isTypedArrayView(st.vm, obj) {
		var tLen int64
		if err := tryJS(st.vm, func() {
			if lv := obj.Get("length"); lv != nil && !goja.IsUndefined(lv) {
				tLen = lv.ToInteger()
			}
		}); err != nil {
			return nil, err
		}
		if tLen < 0 || tLen > maxExportArrayLen {
			if st.forLog {
				return fmt.Sprintf("[%s(%d) exceeds max %d]", className, tLen, maxExportArrayLen), nil
			}
			return nil, fmt.Errorf("RangeError: exported %s length %d exceeds max %d", className, tLen, maxExportArrayLen)
		}
		out := make([]any, tLen)
		for i := int64(0); i < tLen; i++ {
			var elem goja.Value
			if err := tryJS(st.vm, func() { elem = obj.Get(strconv.FormatInt(i, 10)) }); err != nil {
				return nil, err
			}
			v, err := st.exportVal(elem, depth+1)
			if err != nil {
				return nil, err
			}
			out[i] = v
		}
		return out, nil
	}

	// Plain Objects, Error objects, ECMAScript Proxies (`process.env`, `P`/`payloads`), DynamicObjects, and Go maps.
	var keys []string
	if err := tryJS(st.vm, func() {
		keys = obj.Keys()
		if len(keys) == 0 {
			// Fallback for Proxy wrappers if Goja's Proxy.Keys() did not enumerate target keys.
			if p, ok := obj.Export().(goja.Proxy); ok && p.Target() != nil {
				keys = p.Target().Keys()
			}
		}
	}); err != nil {
		return nil, err
	}

	out := make(map[string]any, len(keys))
	for _, k := range keys {
		st.nodes++
		if st.nodes > maxExportTotalNodes {
			if st.forLog {
				out["__truncated__"] = "[MaxItems]"
				break
			}
			return nil, fmt.Errorf("RangeError: exported object exceeds max %d total properties", maxExportTotalNodes)
		}
		var propVal goja.Value
		if err := tryJS(st.vm, func() { propVal = obj.Get(k) }); err != nil {
			if st.forLog {
				rethrowIfInterrupted(err)
				out[k] = "[GetterError: " + output.SliceUTF8Bytes(safeErrString(err), 120) + "]"
				continue
			}
			return nil, err
		}
		if propVal == nil || goja.IsUndefined(propVal) {
			out[k] = nil
			continue
		}
		if propObj, ok := propVal.(*goja.Object); ok {
			if _, isFn := goja.AssertFunction(propObj); isFn {
				// Omit function methods on objects (matching JSON.stringify, e.g. `process.cwd`) so result stays JSON-serializable.
				continue
			}
		}
		if !st.forLog && propVal.ExportType() == reflect.TypeOf((*goja.Symbol)(nil)) {
			continue
		}
		v, err := st.exportVal(propVal, depth+1)
		if err != nil {
			if st.forLog {
				rethrowIfInterrupted(err)
				out[k] = "[GetterError: " + output.SliceUTF8Bytes(safeErrString(err), 120) + "]"
				continue
			}
			return nil, err
		}
		out[k] = v
	}

	// Standard JS `new Error("msg")` has non-enumerable `name` & `message`; surface them when no enumerable keys exist.
	if className == "Error" && len(out) == 0 {
		_ = tryJS(st.vm, func() {
			if n := obj.Get("name"); n != nil && !goja.IsUndefined(n) && !goja.IsNull(n) {
				out["name"] = n.String()
			}
			if m := obj.Get("message"); m != nil && !goja.IsUndefined(m) && !goja.IsNull(m) {
				out["message"] = m.String()
			}
		})
	}

	return out, nil
}

func (st *exportState) tryExtractClonedBytes(obj *goja.Object) ([]byte, bool, error) {
	var rawExp any
	var hasDirectExp bool
	if err := tryJS(st.vm, func() {
		et := obj.ExportType()
		if et == reflect.TypeOf(goja.ArrayBuffer{}) || et == reflect.TypeOf([]byte(nil)) {
			rawExp = obj.Export()
			hasDirectExp = true
		}
	}); err != nil {
		return nil, false, err
	}
	if hasDirectExp {
		switch b := rawExp.(type) {
		case goja.ArrayBuffer:
			bs := b.Bytes()
			if len(bs) > maxExportByteSlice {
				return nil, false, fmt.Errorf("RangeError: exported ArrayBuffer byteLength %d exceeds max %d", len(bs), maxExportByteSlice)
			}
			return bytes.Clone(bs), true, nil
		case []byte:
			if len(b) > maxExportByteSlice {
				return nil, false, fmt.Errorf("RangeError: exported Uint8Array byteLength %d exceeds max %d", len(b), maxExportByteSlice)
			}
			return bytes.Clone(b), true, nil
		}
	}

	// Subclasses like `Buffer extends Uint8Array` or `DataView`: check prototype constructor without invoking Proxy traps on plain objects.
	var viewBytes []byte
	var matched bool
	var rangeErr error
	if err := tryJS(st.vm, func() {
		if obj.ExportType() == reflect.TypeOf(goja.Proxy{}) {
			return
		}
		cn := obj.ClassName()
		if cn != "Uint8Array" && cn != "Uint8ClampedArray" && cn != "DataView" {
			if cn != "Object" {
				return
			}
			proto := obj.Prototype()
			if proto == nil {
				return
			}
			ctor := proto.Get("constructor")
			if ctor == nil || goja.IsUndefined(ctor) || goja.IsNull(ctor) {
				return
			}
			ctorObj, ok := ctor.(*goja.Object)
			if !ok {
				return
			}
			nameVal := ctorObj.Get("name")
			if nameVal == nil || goja.IsUndefined(nameVal) {
				return
			}
			cname := nameVal.String()
			if cname != "Buffer" && cname != "Uint8Array" && cname != "Uint8ClampedArray" && cname != "DataView" {
				return
			}
		}
		bufVal := obj.Get("buffer")
		if bufVal == nil || goja.IsUndefined(bufVal) {
			return
		}
		ab, ok := bufVal.Export().(goja.ArrayBuffer)
		if !ok {
			return
		}
		raw := ab.Bytes()
		offset := 0
		length := len(raw)
		if offVal := obj.Get("byteOffset"); offVal != nil && !goja.IsUndefined(offVal) {
			offset = int(offVal.ToInteger())
		}
		if lenVal := obj.Get("byteLength"); lenVal != nil && !goja.IsUndefined(lenVal) {
			length = int(lenVal.ToInteger())
		}
		if offset >= 0 && length >= 0 && offset <= len(raw) && length <= len(raw)-offset {
			if length > maxExportByteSlice {
				rangeErr = fmt.Errorf("RangeError: exported Buffer byteLength %d exceeds max %d", length, maxExportByteSlice)
				return
			}
			viewBytes = bytes.Clone(raw[offset : offset+length])
			matched = true
		}
	}); err != nil {
		return nil, false, err
	}
	if rangeErr != nil {
		return nil, false, rangeErr
	}
	return viewBytes, matched, nil
}

func isTypedArrayView(vm *goja.Runtime, obj *goja.Object) bool {
	var ok bool
	_ = tryJS(vm, func() {
		switch obj.ClassName() {
		case "Int8Array", "Int16Array", "Uint16Array", "Int32Array", "Uint32Array", "Float32Array", "Float64Array", "BigInt64Array", "BigUint64Array":
			ok = true
		}
	})
	return ok
}

package sandbox

import (
	"crypto/md5"
	"crypto/rand"
	"crypto/sha1"
	"crypto/sha256"
	"crypto/sha512"
	"encoding/base64"
	"encoding/binary"
	"encoding/hex"
	"fmt"
	"hash"
	"net/url"
	"strings"
	"unicode/utf16"

	"github.com/dop251/goja"
)

// installStandardGlobals equips the isolated goja VM with standard zero-I/O Node/Web utility globals
// that LLMs routinely expect in TypeScript:
//   - Buffer (utf8, base64, base64url, hex, utf16le/ucs2, latin1/binary, ascii)
//   - btoa / atob
//   - TextEncoder / TextDecoder
//   - crypto (randomUUID, randomBytes, getRandomValues, createHash)
//   - path (join, resolve, dirname, basename, extname, normalize, isAbsolute, parse, posix, win32)
//   - URL & URLSearchParams
//
// None of these perform filesystem or network I/O, preserving full sandbox isolation.
func installStandardGlobals(env *ExecutionEnv) error {
	vm := env.VM
	codec := vm.NewObject()

	_ = codec.Set("encodeString", func(call goja.FunctionCall) goja.Value {
		str := ""
		if len(call.Arguments) > 0 && !goja.IsUndefined(call.Arguments[0]) && !goja.IsNull(call.Arguments[0]) {
			str = call.Arguments[0].String()
		}
		enc := "utf8"
		if len(call.Arguments) > 1 && !goja.IsUndefined(call.Arguments[1]) && !goja.IsNull(call.Arguments[1]) {
			enc = call.Arguments[1].String()
		}
		b, err := encodeStringBytes(str, enc)
		if err != nil {
			panic(vm.NewTypeError(err.Error()))
		}
		return vm.ToValue(vm.NewArrayBuffer(b))
	})

	_ = codec.Set("decodeBytes", func(call goja.FunctionCall) goja.Value {
		var data []byte
		if len(call.Arguments) > 0 {
			data = extractGoBytes(vm, call.Arguments[0])
		}
		enc := "utf8"
		if len(call.Arguments) > 1 && !goja.IsUndefined(call.Arguments[1]) && !goja.IsNull(call.Arguments[1]) {
			enc = call.Arguments[1].String()
		}
		out, err := decodeBytesToString(data, enc)
		if err != nil {
			panic(vm.NewTypeError(err.Error()))
		}
		return vm.ToValue(out)
	})

	_ = codec.Set("hash", func(call goja.FunctionCall) goja.Value {
		alg := "sha256"
		if len(call.Arguments) > 0 {
			alg = strings.ToLower(strings.TrimSpace(call.Arguments[0].String()))
		}
		var data []byte
		if len(call.Arguments) > 1 {
			data = extractGoBytes(vm, call.Arguments[1])
		}
		var h hash.Hash
		switch alg {
		case "sha256", "sha-256":
			h = sha256.New()
		case "sha512", "sha-512":
			h = sha512.New()
		case "sha384", "sha-384":
			h = sha512.New384()
		case "sha1", "sha-1":
			h = sha1.New()
		case "md5":
			h = md5.New()
		default:
			panic(vm.NewTypeError(fmt.Sprintf("crypto.createHash: unsupported algorithm '%s'", alg)))
		}
		_, _ = h.Write(data)
		return vm.ToValue(vm.NewArrayBuffer(h.Sum(nil)))
	})

	_ = codec.Set("randomBytes", func(call goja.FunctionCall) goja.Value {
		size := 16
		if len(call.Arguments) > 0 {
			size = int(call.Arguments[0].ToInteger())
		}
		if size < 0 || size > 4*1024*1024 {
			panic(vm.NewTypeError("crypto.randomBytes: size must be between 0 and 4194304"))
		}
		buf := make([]byte, size)
		_, _ = rand.Read(buf)
		return vm.ToValue(vm.NewArrayBuffer(buf))
	})

	_ = codec.Set("randomUUID", func(_ goja.FunctionCall) goja.Value {
		var u [16]byte
		_, _ = rand.Read(u[:])
		u[6] = (u[6] & 0x0f) | 0x40 // version 4
		u[8] = (u[8] & 0x3f) | 0x80 // variant 10
		return vm.ToValue(fmt.Sprintf("%08x-%04x-%04x-%04x-%012x", u[0:4], u[4:6], u[6:8], u[8:10], u[10:16]))
	})

	_ = codec.Set("parseURL", func(call goja.FunctionCall) goja.Value {
		raw := ""
		if len(call.Arguments) > 0 {
			raw = call.Arguments[0].String()
		}
		baseStr := ""
		if len(call.Arguments) > 1 && !goja.IsUndefined(call.Arguments[1]) && !goja.IsNull(call.Arguments[1]) {
			baseStr = call.Arguments[1].String()
		}
		var parsed *url.URL
		var err error
		if baseStr != "" {
			baseU, bErr := url.Parse(baseStr)
			if bErr != nil {
				panic(vm.NewTypeError("Invalid base URL: " + baseStr))
			}
			refU, rErr := url.Parse(raw)
			if rErr != nil {
				panic(vm.NewTypeError("Invalid URL: " + raw))
			}
			parsed = baseU.ResolveReference(refU)
		} else {
			parsed, err = url.Parse(raw)
			if err != nil || parsed.Scheme == "" {
				panic(vm.NewTypeError("Invalid URL: " + raw))
			}
		}
		origin := parsed.Scheme + "://" + parsed.Host
		search := ""
		if parsed.RawQuery != "" {
			search = "?" + parsed.RawQuery
		}
		hashFrag := ""
		if parsed.Fragment != "" {
			hashFrag = "#" + parsed.Fragment
		}
		pathname := parsed.EscapedPath()
		if pathname == "" && (parsed.Scheme == "http" || parsed.Scheme == "https") {
			pathname = "/"
		}
		return vm.ToValue(map[string]any{
			"href":     parsed.String(),
			"origin":   origin,
			"protocol": parsed.Scheme + ":",
			"host":     parsed.Host,
			"hostname": parsed.Hostname(),
			"port":     parsed.Port(),
			"pathname": pathname,
			"search":   search,
			"hash":     hashFrag,
			"username": parsed.User.Username(),
		})
	})

	_ = codec.Set("cwd", env.Cwd)
	registerExtraCodecFuncs(env, codec)
	if err := vm.Set("__ptcCodec", codec); err != nil {
		return err
	}

	if _, err := vm.RunProgram(stdlibBootstrapProg); err != nil {
		return err
	}
	return installNodeExtraModules(vm)
}

func extractGoBytes(vm *goja.Runtime, val goja.Value) []byte {
	if val == nil || goja.IsUndefined(val) || goja.IsNull(val) {
		return nil
	}
	exp := val.Export()
	switch b := exp.(type) {
	case goja.ArrayBuffer:
		return b.Bytes()
	case []byte:
		return b
	case string:
		return []byte(b)
	}
	obj := val.ToObject(vm)
	if obj == nil {
		return nil
	}
	// TypedArray / Buffer view: slice exact [byteOffset, byteOffset + byteLength]
	if bufVal := obj.Get("buffer"); bufVal != nil && !goja.IsUndefined(bufVal) {
		if ab, ok := bufVal.Export().(goja.ArrayBuffer); ok {
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
				return raw[offset : offset+length]
			}
			return nil
		}
	}
	if lenVal := obj.Get("length"); lenVal != nil && !goja.IsUndefined(lenVal) {
		n := int(lenVal.ToInteger())
		if n >= 0 && n <= 16*1024*1024 {
			out := make([]byte, n)
			for i := 0; i < n; i++ {
				if item := obj.Get(fmt.Sprintf("%d", i)); item != nil && !goja.IsUndefined(item) {
					out[i] = byte(item.ToInteger() & 0xff)
				}
			}
			return out
		}
	}
	return nil
}

func normalizeEncodingName(enc string) string {
	e := strings.ToLower(strings.TrimSpace(enc))
	e = strings.ReplaceAll(e, "-", "")
	e = strings.ReplaceAll(e, "_", "")
	if e == "" {
		return "utf8"
	}
	return e
}

func encodeStringBytes(str, enc string) ([]byte, error) {
	switch normalizeEncodingName(enc) {
	case "utf8":
		return []byte(str), nil
	case "ascii":
		out := make([]byte, len(str))
		for i := 0; i < len(str); i++ {
			out[i] = str[i] & 0x7f
		}
		return out, nil
	case "latin1", "binary":
		runes := []rune(str)
		out := make([]byte, len(runes))
		for i, r := range runes {
			out[i] = byte(r & 0xff)
		}
		return out, nil
	case "utf16le", "ucs2":
		u16 := utf16.Encode([]rune(str))
		out := make([]byte, len(u16)*2)
		for i, v := range u16 {
			binary.LittleEndian.PutUint16(out[i*2:], v)
		}
		return out, nil
	case "base64":
		cleaned := stripBase64Whitespace(str)
		// Convert URL-safe chars if passed to standard base64 (Node.js Buffer.from compatibility)
		cleaned = strings.ReplaceAll(strings.ReplaceAll(cleaned, "-", "+"), "_", "/")
		if rem := len(cleaned) % 4; rem != 0 {
			cleaned += strings.Repeat("=", 4-rem)
		}
		b, err := base64.StdEncoding.DecodeString(cleaned)
		if err != nil {
			return nil, fmt.Errorf("Invalid base64 input: %w", err)
		}
		return b, nil
	case "base64url":
		cleaned := stripBase64Whitespace(str)
		cleaned = strings.TrimRight(cleaned, "=")
		cleaned = strings.ReplaceAll(strings.ReplaceAll(cleaned, "+", "-"), "/", "_")
		b, err := base64.RawURLEncoding.DecodeString(cleaned)
		if err != nil {
			return nil, fmt.Errorf("Invalid base64url input: %w", err)
		}
		return b, nil
	case "hex":
		cleaned := strings.TrimSpace(str)
		if len(cleaned)%2 == 1 {
			cleaned = cleaned[:len(cleaned)-1] // Node.js Buffer.from('abc', 'hex') truncates trailing odd nibble
		}
		b, err := hex.DecodeString(cleaned)
		if err != nil {
			return nil, fmt.Errorf("Invalid hex input: %w", err)
		}
		return b, nil
	default:
		return nil, fmt.Errorf("Unknown encoding: %s", enc)
	}
}

func decodeBytesToString(data []byte, enc string) (string, error) {
	switch normalizeEncodingName(enc) {
	case "utf8":
		return strings.ToValidUTF8(string(data), "�"), nil
	case "base64":
		return base64.StdEncoding.EncodeToString(data), nil
	case "base64url":
		return base64.RawURLEncoding.EncodeToString(data), nil
	case "hex":
		return hex.EncodeToString(data), nil
	case "ascii":
		runes := make([]rune, len(data))
		for i, b := range data {
			runes[i] = rune(b & 0x7f)
		}
		return string(runes), nil
	case "latin1", "binary":
		runes := make([]rune, len(data))
		for i, b := range data {
			runes[i] = rune(b)
		}
		return string(runes), nil
	case "utf16le", "ucs2":
		n := len(data) / 2
		u16 := make([]uint16, n)
		for i := 0; i < n; i++ {
			u16[i] = binary.LittleEndian.Uint16(data[i*2:])
		}
		return string(utf16.Decode(u16)), nil
	default:
		return "", fmt.Errorf("Unknown encoding: %s", enc)
	}
}

func stripBase64Whitespace(s string) string {
	var b strings.Builder
	b.Grow(len(s))
	for i := 0; i < len(s); i++ {
		c := s[i]
		if c == ' ' || c == '\n' || c == '\r' || c == '\t' {
			continue
		}
		b.WriteByte(c)
	}
	return b.String()
}

var stdlibBootstrapProg = goja.MustCompile("stdlib.js", stdlibBootstrapJS, false)

const stdlibBootstrapJS = `(function() {
	const codec = globalThis.__ptcCodec;

	class Buffer extends Uint8Array {
		static from(value, encodingOrOffset, length) {
			if (typeof value === "string") {
				const ab = codec.encodeString(value, encodingOrOffset || "utf8");
				return new Buffer(ab);
			}
			if (value instanceof ArrayBuffer) {
				const off = typeof encodingOrOffset === "number" ? encodingOrOffset : 0;
				const len = typeof length === "number" ? length : value.byteLength - off;
				const copy = value.slice(off, off + len);
				return new Buffer(copy);
			}
			if (ArrayBuffer.isView(value)) {
				const copy = new Uint8Array(value.buffer, value.byteOffset, value.byteLength).slice();
				return new Buffer(copy.buffer);
			}
			if (Array.isArray(value) || (value && typeof value.length === "number")) {
				const u8 = Uint8Array.from(value);
				return new Buffer(u8.buffer);
			}
			if (value && value.type === "Buffer" && Array.isArray(value.data)) {
				const u8 = Uint8Array.from(value.data);
				return new Buffer(u8.buffer);
			}
			throw new TypeError("Buffer.from: unsupported input type");
		}

		static alloc(size, fill, encoding) {
			const n = Math.max(0, Number(size) | 0);
			const buf = new Buffer(n);
			if (fill !== undefined && n > 0) {
				if (typeof fill === "number") {
					buf.fill(fill & 0xff);
				} else if (typeof fill === "string" && fill.length > 0) {
					const pat = Buffer.from(fill, encoding || "utf8");
					if (pat.length > 0) {
						for (let i = 0; i < n; i++) buf[i] = pat[i % pat.length];
					}
				} else if (ArrayBuffer.isView(fill) && fill.byteLength > 0) {
					const pat = new Uint8Array(fill.buffer, fill.byteOffset, fill.byteLength);
					for (let i = 0; i < n; i++) buf[i] = pat[i % pat.length];
				}
			}
			return buf;
		}

		static allocUnsafe(size) {
			return Buffer.alloc(size);
		}

		static isBuffer(obj) {
			return obj instanceof Buffer;
		}

		static isEncoding(enc) {
			if (typeof enc !== "string") return false;
			const norm = enc.toLowerCase().replace(/[-_]/g, "");
			return ["utf8", "ascii", "latin1", "binary", "base64", "base64url", "hex", "utf16le", "ucs2"].includes(norm);
		}

		static byteLength(val, encoding) {
			if (typeof val === "string") {
				return codec.encodeString(val, encoding || "utf8").byteLength;
			}
			if (ArrayBuffer.isView(val) || val instanceof ArrayBuffer) {
				return val.byteLength;
			}
			return 0;
		}

		static concat(list, totalLength) {
			if (!Array.isArray(list)) throw new TypeError("Buffer.concat requires an array");
			if (list.length === 0) return Buffer.alloc(0);
			let total = 0;
			for (const item of list) total += item.length;
			if (typeof totalLength === "number") total = Math.max(0, totalLength | 0);
			const out = Buffer.alloc(total);
			let offset = 0;
			for (const item of list) {
				if (offset >= total) break;
				const sliceLen = Math.min(item.length, total - offset);
				out.set(item.subarray(0, sliceLen), offset);
				offset += sliceLen;
			}
			return out;
		}

		static compare(a, b) {
			const len = Math.min(a.length, b.length);
			for (let i = 0; i < len; i++) {
				if (a[i] !== b[i]) return a[i] < b[i] ? -1 : 1;
			}
			return a.length === b.length ? 0 : (a.length < b.length ? -1 : 1);
		}

		toString(encoding, start, end) {
			const s = start === undefined ? 0 : Math.max(0, start | 0);
			const e = end === undefined ? this.length : Math.min(this.length, end | 0);
			const view = (s === 0 && e === this.length) ? this : this.subarray(s, e);
			return codec.decodeBytes(view, encoding || "utf8");
		}

		slice(start, end) {
			const sub = super.subarray(start, end);
			return new Buffer(sub.buffer, sub.byteOffset, sub.byteLength);
		}

		subarray(start, end) {
			const sub = super.subarray(start, end);
			return new Buffer(sub.buffer, sub.byteOffset, sub.byteLength);
		}

		equals(other) {
			if (!(other instanceof Uint8Array)) return false;
			return Buffer.compare(this, other) === 0;
		}

		write(str, offset, length, encoding) {
			let off = 0;
			let len = this.length;
			let enc = "utf8";
			if (typeof offset === "string") {
				enc = offset;
			} else if (typeof offset === "number") {
				off = offset | 0;
				if (typeof length === "string") {
					enc = length;
				} else if (typeof length === "number") {
					len = length | 0;
					if (typeof encoding === "string") enc = encoding;
				}
			}
			const src = new Uint8Array(codec.encodeString(String(str), enc));
			const n = Math.max(0, Math.min(src.length, len, this.length - off));
			if (n > 0) this.set(src.subarray(0, n), off);
			return n;
		}

		toJSON() {
			return { type: "Buffer", data: Array.from(this) };
		}
	}

	globalThis.Buffer = Buffer;

	globalThis.btoa = function btoa(str) {
		const s = String(str);
		for (let i = 0; i < s.length; i++) {
			if (s.charCodeAt(i) > 0xff) {
				throw new Error("InvalidCharacterError: btoa input contains characters outside Latin1 range (use Buffer.from(str, 'utf8').toString('base64') for UTF-8)");
			}
		}
		return Buffer.from(s, "latin1").toString("base64");
	};

	globalThis.atob = function atob(b64) {
		return Buffer.from(String(b64), "base64").toString("latin1");
	};

	globalThis.TextEncoder = class TextEncoder {
		get encoding() { return "utf-8"; }
		encode(input = "") {
			return new Uint8Array(codec.encodeString(String(input), "utf8"));
		}
	};

	globalThis.TextDecoder = class TextDecoder {
		constructor(label = "utf-8") {
			this.encoding = String(label).toLowerCase();
		}
		decode(input) {
			if (!input) return "";
			return codec.decodeBytes(input, this.encoding);
		}
	};

	// Pure-string path utilities (zero filesystem I/O)
	function makePathAPI(isWin) {
		const sep = isWin ? "\\" : "/";
		const delimiter = isWin ? ";" : ":";
		const toPosix = (p) => String(p || "").replace(/\\/g, "/");
		const normalizePosix = (p) => {
			let raw = toPosix(p);
			if (!raw) return ".";
			let prefix = "";
			const driveMatch = isWin ? raw.match(/^[A-Za-z]:/) : null;
			if (driveMatch) {
				prefix = driveMatch[0];
				raw = raw.slice(2);
			}
			const isAbs = raw.startsWith("/");
			const parts = raw.split("/");
			const stack = [];
			for (const part of parts) {
				if (!part || part === ".") continue;
				if (part === "..") {
					if (stack.length > 0 && stack[stack.length - 1] !== "..") {
						stack.pop();
					} else if (!isAbs) {
						stack.push("..");
					}
				} else {
					stack.push(part);
				}
			}
			let joined = (isAbs ? "/" : "") + stack.join("/");
			if (!joined) joined = isAbs ? "/" : ".";
			const res = prefix + joined;
			return isWin ? res.replace(/\//g, "\\") : res;
		};

		const isAbsolute = (p) => {
			const s = toPosix(p);
			return s.startsWith("/") || (isWin && /^[A-Za-z]:\//.test(s));
		};

		const join = (...segments) => {
			const valid = segments.filter((s) => s !== undefined && s !== null && String(s) !== "").map(String);
			if (valid.length === 0) return ".";
			return normalizePosix(valid.join("/"));
		};

		const resolve = (...segments) => {
			let resolved = "";
			for (let i = segments.length - 1; i >= 0; i--) {
				const seg = segments[i];
				if (!seg) continue;
				resolved = resolved ? String(seg) + "/" + resolved : String(seg);
				if (isAbsolute(seg)) break;
			}
			if (!isAbsolute(resolved)) {
				const base = codec.cwd || "/";
				resolved = base + "/" + resolved;
			}
			return normalizePosix(resolved);
		};

		const basename = (p, ext) => {
			const s = toPosix(p).replace(/\/+$/, "");
			const idx = s.lastIndexOf("/");
			let base = idx >= 0 ? s.slice(idx + 1) : s;
			if (isWin && /^[A-Za-z]:$/.test(base)) return "";
			if (ext && base.endsWith(ext)) base = base.slice(0, base.length - ext.length);
			return base;
		};

		const dirname = (p) => {
			const s = toPosix(p).replace(/\/+$/, "");
			if (!s) return ".";
			const idx = s.lastIndexOf("/");
			if (idx < 0) return ".";
			if (idx === 0) return sep;
			const head = s.slice(0, idx);
			if (isWin && /^[A-Za-z]:$/.test(head)) return head + sep;
			return isWin ? head.replace(/\//g, "\\") : head;
		};

		const extname = (p) => {
			const base = basename(p);
			const dot = base.lastIndexOf(".");
			return dot <= 0 ? "" : base.slice(dot);
		};

		const parse = (p) => {
			const ext = extname(p);
			const base = basename(p);
			const name = ext ? base.slice(0, base.length - ext.length) : base;
			const dir = dirname(p);
			const s = toPosix(p);
			let root = "";
			if (isWin && /^[A-Za-z]:\//.test(s)) root = s.slice(0, 2) + "\\";
			else if (s.startsWith("/")) root = sep;
			return { root, dir, base, ext, name };
		};

		return { sep, delimiter, normalize: normalizePosix, isAbsolute, join, resolve, basename, dirname, extname, parse };
	}

	const posixPath = makePathAPI(false);
	const winPath = makePathAPI(true);
	const hostIsWin = /^[A-Za-z]:/.test(String(codec.cwd || ""));
	const defaultPath = Object.assign({}, hostIsWin ? winPath : posixPath, { posix: posixPath, win32: winPath });
	globalThis.path = defaultPath;

	// Pure-memory Web/Node crypto utilities
	globalThis.crypto = {
		randomUUID: () => codec.randomUUID(),
		randomBytes: (size) => new Buffer(codec.randomBytes(size)),
		getRandomValues: (typedArray) => {
			if (!ArrayBuffer.isView(typedArray)) throw new TypeError("crypto.getRandomValues expects a TypedArray");
			const rnd = new Uint8Array(codec.randomBytes(typedArray.byteLength));
			new Uint8Array(typedArray.buffer, typedArray.byteOffset, typedArray.byteLength).set(rnd);
			return typedArray;
		},
		createHash: (algorithm) => {
			const chunks = [];
			return {
				update(data, inputEncoding) {
					if (typeof data === "string") {
						chunks.push(Buffer.from(data, inputEncoding || "utf8"));
					} else {
						chunks.push(Buffer.from(data));
					}
					return this;
				},
				digest(outputEncoding) {
					const combined = Buffer.concat(chunks);
					const rawHash = new Buffer(codec.hash(algorithm, combined));
					return outputEncoding ? rawHash.toString(outputEncoding) : rawHash;
				},
			};
		},
	};

	// URLSearchParams & URL
	class URLSearchParams {
		constructor(init) {
			this._entries = [];
			if (!init) return;
			if (typeof init === "string") {
				const q = init.startsWith("?") ? init.slice(1) : init;
				if (!q) return;
				for (const pair of q.split("&")) {
					if (!pair) continue;
					const eq = pair.indexOf("=");
					const k = eq >= 0 ? pair.slice(0, eq) : pair;
					const v = eq >= 0 ? pair.slice(eq + 1) : "";
					this.append(decodeURIComponent(k.replace(/\+/g, " ")), decodeURIComponent(v.replace(/\+/g, " ")));
				}
			} else if (Array.isArray(init)) {
				for (const [k, v] of init) this.append(k, v);
			} else if (init instanceof URLSearchParams) {
				for (const [k, v] of init._entries) this.append(k, v);
			} else if (typeof init === "object") {
				for (const k of Object.keys(init)) this.append(k, init[k]);
			}
		}
		append(name, value) { this._entries.push([String(name), String(value)]); }
		set(name, value) {
			const k = String(name);
			const v = String(value);
			let replaced = false;
			this._entries = this._entries.filter(([ek, ev]) => {
				if (ek !== k) return true;
				if (!replaced) { replaced = true; return true; }
				return false;
			}).map(([ek, ev]) => (ek === k ? [k, v] : [ek, ev]));
			if (!replaced) this._entries.push([k, v]);
		}
		get(name) {
			const found = this._entries.find(([k]) => k === String(name));
			return found ? found[1] : null;
		}
		getAll(name) { return this._entries.filter(([k]) => k === String(name)).map(([, v]) => v); }
		has(name) { return this._entries.some(([k]) => k === String(name)); }
		delete(name) { this._entries = this._entries.filter(([k]) => k !== String(name)); }
		entries() { return this._entries[Symbol.iterator](); }
		keys() { return this._entries.map(([k]) => k)[Symbol.iterator](); }
		values() { return this._entries.map(([, v]) => v)[Symbol.iterator](); }
		[Symbol.iterator]() { return this.entries(); }
		forEach(cb) { for (const [k, v] of this._entries) cb(v, k, this); }
		toString() {
			return this._entries
				.map(([k, v]) => encodeURIComponent(k).replace(/%20/g, "+") + "=" + encodeURIComponent(v).replace(/%20/g, "+"))
				.join("&");
		}
	}

	class URL {
		constructor(input, base) {
			const info = codec.parseURL(String(input), base !== undefined ? String(base) : undefined);
			Object.assign(this, info);
			this.searchParams = new URLSearchParams(info.search);
		}
		toString() {
			const qs = this.searchParams.toString();
			const baseHref = this.origin + this.pathname;
			return baseHref + (qs ? "?" + qs : "") + (this.hash || "");
		}
		toJSON() { return this.toString(); }
	}

	globalThis.URLSearchParams = URLSearchParams;
	globalThis.URL = URL;
})()`

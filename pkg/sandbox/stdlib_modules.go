package sandbox

import (
	"bytes"
	"compress/flate"
	"compress/gzip"
	"compress/zlib"
	"crypto/hmac"
	"crypto/md5"
	"crypto/rand"
	"crypto/sha1"
	"crypto/sha256"
	"crypto/sha512"
	"crypto/subtle"
	"fmt"
	"hash"
	"io"
	"math/big"
	"strings"
	"time"

	"github.com/dop251/goja"
)

const defaultMaxZlibOutput = 32 * 1024 * 1024 // 32 MB decompression safety cap

func newHashByName(vm *goja.Runtime, caller, alg string) func() hash.Hash {
	switch strings.ToLower(strings.TrimSpace(alg)) {
	case "sha256", "sha-256":
		return sha256.New
	case "sha512", "sha-512":
		return sha512.New
	case "sha384", "sha-384":
		return sha512.New384
	case "sha1", "sha-1":
		return sha1.New
	case "md5":
		return md5.New
	default:
		panic(vm.NewTypeError(fmt.Sprintf("%s: unsupported algorithm '%s'", caller, alg)))
	}
}

func registerExtraCodecFuncs(env *ExecutionEnv, codec *goja.Object) {
	vm := env.VM
	bootUnixMs := float64(env.startedAt.UnixMicro()) / 1000.0

	_ = codec.Set("hmac", func(call goja.FunctionCall) goja.Value {
		alg := "sha256"
		if len(call.Arguments) > 0 {
			alg = call.Arguments[0].String()
		}
		var key, data []byte
		if len(call.Arguments) > 1 {
			key = extractGoBytes(vm, call.Arguments[1])
		}
		if len(call.Arguments) > 2 {
			data = extractGoBytes(vm, call.Arguments[2])
		}
		mac := hmac.New(newHashByName(vm, "crypto.createHmac", alg), key)
		_, _ = mac.Write(data)
		return vm.ToValue(vm.NewArrayBuffer(mac.Sum(nil)))
	})

	_ = codec.Set("timingSafeEqual", func(call goja.FunctionCall) goja.Value {
		if len(call.Arguments) < 2 {
			panic(vm.NewTypeError("crypto.timingSafeEqual requires (a, b)"))
		}
		a := extractGoBytes(vm, call.Arguments[0])
		b := extractGoBytes(vm, call.Arguments[1])
		if len(a) != len(b) {
			panic(newJSRangeError(vm, "Input buffers must have the same byte length"))
		}
		return vm.ToValue(subtle.ConstantTimeCompare(a, b) == 1)
	})

	_ = codec.Set("randomInt", func(call goja.FunctionCall) goja.Value {
		if len(call.Arguments) == 0 {
			panic(vm.NewTypeError("crypto.randomInt requires (max) or (min, max)"))
		}
		var minVal, maxVal int64
		if len(call.Arguments) == 1 || goja.IsUndefined(call.Arguments[1]) {
			minVal = 0
			maxVal = call.Arguments[0].ToInteger()
		} else {
			minVal = call.Arguments[0].ToInteger()
			maxVal = call.Arguments[1].ToInteger()
		}
		if maxVal <= minVal || (maxVal-minVal) > (1<<48) {
			panic(newJSRangeError(vm, fmt.Sprintf("crypto.randomInt: invalid range [%d, %d)", minVal, maxVal)))
		}
		nBig, err := rand.Int(rand.Reader, big.NewInt(maxVal-minVal))
		if err != nil {
			panic(vm.NewGoError(err))
		}
		return vm.ToValue(minVal + nBig.Int64())
	})

	_ = codec.Set("zlibOp", func(call goja.FunctionCall) goja.Value {
		mode := ""
		if len(call.Arguments) > 0 {
			mode = call.Arguments[0].String()
		}
		var data []byte
		if len(call.Arguments) > 1 {
			data = extractGoBytes(vm, call.Arguments[1])
		}
		maxOut := int64(defaultMaxZlibOutput)
		if len(call.Arguments) > 2 && !goja.IsUndefined(call.Arguments[2]) && !goja.IsNull(call.Arguments[2]) {
			if v := call.Arguments[2].ToInteger(); v > 0 && v <= 64*1024*1024 {
				maxOut = v
			}
		}
		out, err := executeZlibOp(mode, data, maxOut)
		if err != nil {
			panic(vm.NewGoError(err))
		}
		return vm.ToValue(vm.NewArrayBuffer(out))
	})

	_ = codec.Set("timeOrigin", bootUnixMs)
	_ = codec.Set("perfNow", func(_ goja.FunctionCall) goja.Value {
		return vm.ToValue(float64(time.Since(env.startedAt).Microseconds()) / 1000.0)
	})
}

func newJSRangeError(vm *goja.Runtime, msg string) goja.Value {
	if ctor, ok := goja.AssertConstructor(vm.Get("RangeError")); ok && ctor != nil {
		if obj, err := ctor(nil, vm.ToValue(msg)); err == nil {
			return obj
		}
	}
	return vm.NewTypeError("RangeError: " + msg)
}

func readBounded(r io.Reader, maxOut int64) ([]byte, error) {
	lr := &io.LimitedReader{R: r, N: maxOut + 1}
	out, err := io.ReadAll(lr)
	if err != nil {
		return nil, err
	}
	if int64(len(out)) > maxOut {
		return nil, fmt.Errorf("zlib: decompressed output exceeds maxOutputLength (%d bytes)", maxOut)
	}
	return out, nil
}

func executeZlibOp(mode string, data []byte, maxOut int64) ([]byte, error) {
	switch mode {
	case "gzip":
		var buf bytes.Buffer
		w := gzip.NewWriter(&buf)
		if _, err := w.Write(data); err != nil {
			return nil, err
		}
		if err := w.Close(); err != nil {
			return nil, err
		}
		return buf.Bytes(), nil
	case "gunzip":
		r, err := gzip.NewReader(bytes.NewReader(data))
		if err != nil {
			return nil, err
		}
		defer r.Close()
		return readBounded(r, maxOut)
	case "deflate":
		var buf bytes.Buffer
		w := zlib.NewWriter(&buf)
		if _, err := w.Write(data); err != nil {
			return nil, err
		}
		if err := w.Close(); err != nil {
			return nil, err
		}
		return buf.Bytes(), nil
	case "inflate":
		r, err := zlib.NewReader(bytes.NewReader(data))
		if err != nil {
			return nil, err
		}
		defer r.Close()
		return readBounded(r, maxOut)
	case "deflateRaw":
		var buf bytes.Buffer
		w, err := flate.NewWriter(&buf, flate.DefaultCompression)
		if err != nil {
			return nil, err
		}
		if _, err := w.Write(data); err != nil {
			return nil, err
		}
		if err := w.Close(); err != nil {
			return nil, err
		}
		return buf.Bytes(), nil
	case "inflateRaw":
		r := flate.NewReader(bytes.NewReader(data))
		defer r.Close()
		return readBounded(r, maxOut)
	case "unzip":
		if len(data) >= 2 && data[0] == 0x1f && data[1] == 0x8b {
			return executeZlibOp("gunzip", data, maxOut)
		}
		return executeZlibOp("inflate", data, maxOut)
	default:
		return nil, fmt.Errorf("zlib: unknown operation %q", mode)
	}
}

func installNodeExtraModules(vm *goja.Runtime) error {
	_, err := vm.RunProgram(stdlibExtraModulesProg)
	return err
}

var stdlibExtraModulesProg = goja.MustCompile("stdlib_modules.js", `(function() {
	const codec = globalThis.__ptcCodec;
	delete globalThis.__ptcCodec;

	// --- 1. Complete Node.js Buffer binary read/write, search, copy & byte-swap methods ---
	const B8 = BigInt(8), B32 = BigInt(32), B64 = BigInt(64);
	const MASK32 = BigInt(0xffffffff);
	const MASK64 = (BigInt(1) << B64) - BigInt(1);
	const SIGN64 = BigInt(1) << BigInt(63);
	const bp = Buffer.prototype;

	Buffer.allocUnsafeSlow = (size) => Buffer.alloc(size);

	bp._check = function(offset, byteLen) {
		const off = offset === undefined ? 0 : Number(offset);
		if (!Number.isInteger(off) || off < 0 || off + byteLen > this.length) {
			throw new RangeError("ERR_OUT_OF_RANGE: offset " + String(offset) + " is out of bounds for Buffer length " + this.length);
		}
		return off;
	};
	bp._dv = function() {
		if (!this.__dv || this.__dv.buffer !== this.buffer || this.__dv.byteOffset !== this.byteOffset || this.__dv.byteLength !== this.byteLength) {
			Object.defineProperty(this, "__dv", { value: new DataView(this.buffer, this.byteOffset, this.byteLength), writable: true, configurable: true });
		}
		return this.__dv;
	};

	bp.readUInt8 = bp.readUint8 = function(off = 0) { const o = this._check(off, 1); return this[o]; };
	bp.readInt8 = function(off = 0) { const o = this._check(off, 1); return this._dv().getInt8(o); };
	bp.readUInt16LE = bp.readUint16LE = function(off = 0) { const o = this._check(off, 2); return this._dv().getUint16(o, true); };
	bp.readUInt16BE = bp.readUint16BE = function(off = 0) { const o = this._check(off, 2); return this._dv().getUint16(o, false); };
	bp.readInt16LE = function(off = 0) { const o = this._check(off, 2); return this._dv().getInt16(o, true); };
	bp.readInt16BE = function(off = 0) { const o = this._check(off, 2); return this._dv().getInt16(o, false); };
	bp.readUInt32LE = bp.readUint32LE = function(off = 0) { const o = this._check(off, 4); return this._dv().getUint32(o, true); };
	bp.readUInt32BE = bp.readUint32BE = function(off = 0) { const o = this._check(off, 4); return this._dv().getUint32(o, false); };
	bp.readInt32LE = function(off = 0) { const o = this._check(off, 4); return this._dv().getInt32(o, true); };
	bp.readInt32BE = function(off = 0) { const o = this._check(off, 4); return this._dv().getInt32(o, false); };
	bp.readFloatLE = function(off = 0) { const o = this._check(off, 4); return this._dv().getFloat32(o, true); };
	bp.readFloatBE = function(off = 0) { const o = this._check(off, 4); return this._dv().getFloat32(o, false); };
	bp.readDoubleLE = function(off = 0) { const o = this._check(off, 8); return this._dv().getFloat64(o, true); };
	bp.readDoubleBE = function(off = 0) { const o = this._check(off, 8); return this._dv().getFloat64(o, false); };

	bp.readBigUInt64LE = bp.readBigUint64LE = function(off = 0) {
		const o = this._check(off, 8);
		const dv = this._dv();
		return (BigInt(dv.getUint32(o + 4, true)) << B32) | BigInt(dv.getUint32(o, true));
	};
	bp.readBigUInt64BE = bp.readBigUint64BE = function(off = 0) {
		const o = this._check(off, 8);
		const dv = this._dv();
		return (BigInt(dv.getUint32(o, false)) << B32) | BigInt(dv.getUint32(o + 4, false));
	};
	bp.readBigInt64LE = function(off = 0) {
		const u = this.readBigUInt64LE(off);
		return u >= SIGN64 ? u - (SIGN64 << BigInt(1)) : u;
	};
	bp.readBigInt64BE = function(off = 0) {
		const u = this.readBigUInt64BE(off);
		return u >= SIGN64 ? u - (SIGN64 << BigInt(1)) : u;
	};

	bp.readUIntLE = bp.readUintLE = function(off, byteLength) {
		const bl = Number(byteLength);
		if (!Number.isInteger(bl) || bl < 1 || bl > 6) throw new RangeError("byteLength must be >= 1 and <= 6");
		const o = this._check(off, bl);
		let val = 0, mul = 1;
		for (let i = 0; i < bl; i++) { val += this[o + i] * mul; mul *= 256; }
		return val;
	};
	bp.readUIntBE = bp.readUintBE = function(off, byteLength) {
		const bl = Number(byteLength);
		if (!Number.isInteger(bl) || bl < 1 || bl > 6) throw new RangeError("byteLength must be >= 1 and <= 6");
		const o = this._check(off, bl);
		let val = 0;
		for (let i = 0; i < bl; i++) val = val * 256 + this[o + i];
		return val;
	};
	bp.readIntLE = function(off, byteLength) {
		const val = this.readUIntLE(off, byteLength);
		const limit = Math.pow(2, 8 * byteLength - 1);
		return val >= limit ? val - limit * 2 : val;
	};
	bp.readIntBE = function(off, byteLength) {
		const val = this.readUIntBE(off, byteLength);
		const limit = Math.pow(2, 8 * byteLength - 1);
		return val >= limit ? val - limit * 2 : val;
	};

	bp.writeUInt8 = bp.writeUint8 = function(val, off = 0) { const o = this._check(off, 1); this[o] = Number(val) & 0xff; return o + 1; };
	bp.writeInt8 = function(val, off = 0) { const o = this._check(off, 1); this._dv().setInt8(o, Number(val)); return o + 1; };
	bp.writeUInt16LE = bp.writeUint16LE = function(val, off = 0) { const o = this._check(off, 2); this._dv().setUint16(o, Number(val), true); return o + 2; };
	bp.writeUInt16BE = bp.writeUint16BE = function(val, off = 0) { const o = this._check(off, 2); this._dv().setUint16(o, Number(val), false); return o + 2; };
	bp.writeInt16LE = function(val, off = 0) { const o = this._check(off, 2); this._dv().setInt16(o, Number(val), true); return o + 2; };
	bp.writeInt16BE = function(val, off = 0) { const o = this._check(off, 2); this._dv().setInt16(o, Number(val), false); return o + 2; };
	bp.writeUInt32LE = bp.writeUint32LE = function(val, off = 0) { const o = this._check(off, 4); this._dv().setUint32(o, Number(val) >>> 0, true); return o + 4; };
	bp.writeUInt32BE = bp.writeUint32BE = function(val, off = 0) { const o = this._check(off, 4); this._dv().setUint32(o, Number(val) >>> 0, false); return o + 4; };
	bp.writeInt32LE = function(val, off = 0) { const o = this._check(off, 4); this._dv().setInt32(o, Number(val) | 0, true); return o + 4; };
	bp.writeInt32BE = function(val, off = 0) { const o = this._check(off, 4); this._dv().setInt32(o, Number(val) | 0, false); return o + 4; };
	bp.writeFloatLE = function(val, off = 0) { const o = this._check(off, 4); this._dv().setFloat32(o, Number(val), true); return o + 4; };
	bp.writeFloatBE = function(val, off = 0) { const o = this._check(off, 4); this._dv().setFloat32(o, Number(val), false); return o + 4; };
	bp.writeDoubleLE = function(val, off = 0) { const o = this._check(off, 8); this._dv().setFloat64(o, Number(val), true); return o + 8; };
	bp.writeDoubleBE = function(val, off = 0) { const o = this._check(off, 8); this._dv().setFloat64(o, Number(val), false); return o + 8; };

	bp.writeBigUInt64LE = bp.writeBigUint64LE = bp.writeBigInt64LE = function(val, off = 0) {
		const o = this._check(off, 8);
		const v = BigInt(val) & MASK64;
		const dv = this._dv();
		dv.setUint32(o, Number(v & MASK32), true);
		dv.setUint32(o + 4, Number((v >> B32) & MASK32), true);
		return o + 8;
	};
	bp.writeBigUInt64BE = bp.writeBigUint64BE = bp.writeBigInt64BE = function(val, off = 0) {
		const o = this._check(off, 8);
		const v = BigInt(val) & MASK64;
		const dv = this._dv();
		dv.setUint32(o, Number((v >> B32) & MASK32), false);
		dv.setUint32(o + 4, Number(v & MASK32), false);
		return o + 8;
	};
	bp.writeUIntLE = bp.writeUintLE = bp.writeIntLE = function(val, off, byteLength) {
		const bl = Number(byteLength);
		if (!Number.isInteger(bl) || bl < 1 || bl > 6) throw new RangeError("byteLength must be >= 1 and <= 6");
		const o = this._check(off, bl);
		let v = BigInt(Math.trunc(Number(val))) & ((BigInt(1) << BigInt(bl * 8)) - BigInt(1));
		for (let i = 0; i < bl; i++) { this[o + i] = Number(v & BigInt(0xff)); v >>= B8; }
		return o + bl;
	};
	bp.writeUIntBE = bp.writeUintBE = bp.writeIntBE = function(val, off, byteLength) {
		const bl = Number(byteLength);
		if (!Number.isInteger(bl) || bl < 1 || bl > 6) throw new RangeError("byteLength must be >= 1 and <= 6");
		const o = this._check(off, bl);
		let v = BigInt(Math.trunc(Number(val))) & ((BigInt(1) << BigInt(bl * 8)) - BigInt(1));
		for (let i = bl - 1; i >= 0; i--) { this[o + i] = Number(v & BigInt(0xff)); v >>= B8; }
		return o + bl;
	};

	bp.copy = function(target, targetStart = 0, sourceStart = 0, sourceEnd = this.length) {
		const ts = Math.max(0, targetStart | 0);
		const ss = Math.max(0, sourceStart | 0);
		const se = Math.min(this.length, sourceEnd | 0);
		if (se <= ss || ts >= target.length) return 0;
		const n = Math.min(se - ss, target.length - ts);
		target.set(this.subarray(ss, ss + n), ts);
		return n;
	};
	bp.compare = function(target, targetStart = 0, targetEnd = target.length, sourceStart = 0, sourceEnd = this.length) {
		return Buffer.compare(this.subarray(sourceStart, sourceEnd), target.subarray(targetStart, targetEnd));
	};

	function toNeedleBytes(val, enc) {
		if (typeof val === "number") return new Uint8Array([val & 0xff]);
		if (typeof val === "string") return Buffer.from(val, enc || "utf8");
		if (val instanceof Uint8Array) return val;
		if (ArrayBuffer.isView(val)) return new Uint8Array(val.buffer, val.byteOffset, val.byteLength);
		throw new TypeError("value must be a string, number, or Buffer/Uint8Array");
	}
	bp.indexOf = function(val, byteOffset = 0, encoding = "utf8") {
		let off = typeof byteOffset === "string" ? 0 : (byteOffset | 0);
		const enc = typeof byteOffset === "string" ? byteOffset : encoding;
		if (off < 0) off = Math.max(0, this.length + off);
		const needle = toNeedleBytes(val, enc);
		if (needle.length === 0) return Math.min(off, this.length);
		const limit = this.length - needle.length;
		outer: for (let i = off; i <= limit; i++) {
			for (let j = 0; j < needle.length; j++) {
				if (this[i + j] !== needle[j]) continue outer;
			}
			return i;
		}
		return -1;
	};
	bp.lastIndexOf = function(val, byteOffset, encoding = "utf8") {
		const enc = typeof byteOffset === "string" ? byteOffset : encoding;
		const needle = toNeedleBytes(val, enc);
		let off = (byteOffset === undefined || typeof byteOffset === "string") ? this.length - needle.length : (byteOffset | 0);
		if (off < 0) off = this.length + off;
		off = Math.min(off, this.length - needle.length);
		if (needle.length === 0) return Math.max(0, Math.min(this.length, off));
		outer: for (let i = off; i >= 0; i--) {
			for (let j = 0; j < needle.length; j++) {
				if (this[i + j] !== needle[j]) continue outer;
			}
			return i;
		}
		return -1;
	};
	bp.includes = function(val, byteOffset = 0, encoding = "utf8") {
		return this.indexOf(val, byteOffset, encoding) !== -1;
	};
	bp.swap16 = function() {
		if (this.length % 2 !== 0) throw new RangeError("Buffer size must be a multiple of 16-bits");
		for (let i = 0; i < this.length; i += 2) { const t = this[i]; this[i] = this[i + 1]; this[i + 1] = t; }
		return this;
	};
	bp.swap32 = function() {
		if (this.length % 4 !== 0) throw new RangeError("Buffer size must be a multiple of 32-bits");
		for (let i = 0; i < this.length; i += 4) {
			const t0 = this[i], t1 = this[i + 1];
			this[i] = this[i + 3]; this[i + 1] = this[i + 2]; this[i + 2] = t1; this[i + 3] = t0;
		}
		return this;
	};
	bp.swap64 = function() {
		if (this.length % 8 !== 0) throw new RangeError("Buffer size must be a multiple of 64-bits");
		for (let i = 0; i < this.length; i += 8) {
			for (let k = 0; k < 4; k++) { const t = this[i + k]; this[i + k] = this[i + 7 - k]; this[i + 7 - k] = t; }
		}
		return this;
	};

	// --- 2. Extend path (posix, win32, default) with relative, format, toNamespacedPath ---
	function augmentPathObj(pObj, isWin) {
		pObj.format = function(po) {
			if (!po || typeof po !== "object") throw new TypeError("path.format requires an object");
			const dir = po.dir || po.root || "";
			let base = po.base || "";
			if (!base) {
				const name = po.name || "";
				let ext = po.ext || "";
				if (ext && !ext.startsWith(".")) ext = "." + ext;
				base = name + ext;
			}
			if (!dir) return base;
			if (dir === po.root) return dir + base;
			return dir + pObj.sep + base;
		};
		pObj.relative = function(from, to) {
			const f = pObj.resolve(from || "");
			const t = pObj.resolve(to || "");
			const cmp = isWin ? (a, b) => a.toLowerCase() === b.toLowerCase() : (a, b) => a === b;
			if (cmp(f, t)) return "";
			const fNorm = f.replace(/\\/g, "/");
			const tNorm = t.replace(/\\/g, "/");
			if (isWin && fNorm.slice(0, 2).toLowerCase() !== tNorm.slice(0, 2).toLowerCase()) {
				return t;
			}
			const fParts = fNorm.replace(/^[A-Za-z]:/, "").split("/").filter(Boolean);
			const tParts = tNorm.replace(/^[A-Za-z]:/, "").split("/").filter(Boolean);
			let common = 0;
			while (common < fParts.length && common < tParts.length && cmp(fParts[common], tParts[common])) {
				common++;
			}
			const up = Array(fParts.length - common).fill("..");
			const rel = up.concat(tParts.slice(common)).join(pObj.sep);
			return rel;
		};
		pObj.toNamespacedPath = (p) => String(p || "");
	}
	augmentPathObj(globalThis.path.posix, false);
	augmentPathObj(globalThis.path.win32, true);
	const hostWin = globalThis.path.sep === "\\";
	const activeSrc = hostWin ? globalThis.path.win32 : globalThis.path.posix;
	globalThis.path.format = activeSrc.format;
	globalThis.path.relative = activeSrc.relative;
	globalThis.path.toNamespacedPath = activeSrc.toNamespacedPath;

	// --- 3. Extend crypto with createHmac, hash, timingSafeEqual, randomInt, randomFillSync ---
	const origCreateHash = globalThis.crypto.createHash;
	function makeHashState(algorithm, initChunks) {
		const chunks = initChunks ? initChunks.slice() : [];
		return {
			update(data, inputEncoding) {
				chunks.push(typeof data === "string" ? Buffer.from(data, inputEncoding || "utf8") : Buffer.from(data));
				return this;
			},
			digest(outputEncoding) {
				const raw = new Buffer(codec.hash(algorithm, Buffer.concat(chunks)));
				return outputEncoding ? raw.toString(outputEncoding) : raw;
			},
			copy() {
				return makeHashState(algorithm, chunks);
			},
		};
	}
	globalThis.crypto.createHash = (algorithm) => makeHashState(algorithm, null);
	globalThis.crypto.createHmac = (algorithm, key) => {
		const keyBuf = typeof key === "string" ? Buffer.from(key, "utf8") : Buffer.from(key);
		const chunks = [];
		return {
			update(data, inputEncoding) {
				chunks.push(typeof data === "string" ? Buffer.from(data, inputEncoding || "utf8") : Buffer.from(data));
				return this;
			},
			digest(outputEncoding) {
				const raw = new Buffer(codec.hmac(algorithm, keyBuf, Buffer.concat(chunks)));
				return outputEncoding ? raw.toString(outputEncoding) : raw;
			},
		};
	};
	globalThis.crypto.hash = (algorithm, data, outputEncoding = "hex") => {
		return globalThis.crypto.createHash(algorithm).update(data).digest(outputEncoding);
	};
	globalThis.crypto.timingSafeEqual = (a, b) => codec.timingSafeEqual(a, b);
	globalThis.crypto.randomInt = (min, max) => codec.randomInt(min, max);
	globalThis.crypto.randomFillSync = (buf, offset = 0, size) => {
		if (!ArrayBuffer.isView(buf)) throw new TypeError("crypto.randomFillSync expects a Buffer or TypedArray");
		const off = offset | 0;
		const len = size === undefined ? buf.byteLength - off : (size | 0);
		if (off < 0 || len < 0 || off + len > buf.byteLength) throw new RangeError("ERR_OUT_OF_RANGE");
		const rnd = new Uint8Array(codec.randomBytes(len));
		new Uint8Array(buf.buffer, buf.byteOffset + off, len).set(rnd);
		return buf;
	};
	void origCreateHash;

	// --- 4. URL.canParse, URLSearchParams.sort/size, TextEncoder.encodeInto, performance ---
	URL.canParse = function(input, base) {
		try { new URL(input, base); return true; } catch (_) { return false; }
	};
	Object.defineProperty(URLSearchParams.prototype, "size", {
		get() { return this._entries.length; },
		configurable: true,
	});
	URLSearchParams.prototype.sort = function() {
		this._entries.sort((a, b) => (a[0] < b[0] ? -1 : a[0] > b[0] ? 1 : 0));
	};
	TextEncoder.prototype.encodeInto = function(src, dest) {
		const s = String(src || "");
		const encoded = this.encode(s);
		const written = Math.min(encoded.length, dest.length);
		dest.set(encoded.subarray(0, written), 0);
		return { read: s.length, written };
	};

	const performance = Object.freeze({
		timeOrigin: codec.timeOrigin,
		now: () => codec.perfNow(),
	});
	globalThis.performance = performance;
	globalThis.perf_hooks = Object.freeze({ performance });

	// --- 5. zlib (gzip, gunzip, deflate, inflate, deflateRaw, inflateRaw, unzip — sync & async) ---
	function makeZlibSync(mode) {
		return function(data, opts) {
			const buf = typeof data === "string" ? Buffer.from(data, "utf8") : data;
			const maxOut = opts && typeof opts.maxOutputLength === "number" ? opts.maxOutputLength : undefined;
			return new Buffer(codec.zlibOp(mode, buf, maxOut));
		};
	}
	function makeZlibAsync(syncFn) {
		return function(data, optsOrCb, maybeCb) {
			const cb = typeof optsOrCb === "function" ? optsOrCb : (typeof maybeCb === "function" ? maybeCb : null);
			const opts = typeof optsOrCb === "object" ? optsOrCb : undefined;
			if (cb) {
				Promise.resolve().then(() => {
					try { cb(null, syncFn(data, opts)); } catch (e) { cb(e); }
				});
				return;
			}
			return new Promise((resolve, reject) => {
				try { resolve(syncFn(data, opts)); } catch (e) { reject(e); }
			});
		};
	}
	const gzipSync = makeZlibSync("gzip");
	const gunzipSync = makeZlibSync("gunzip");
	const deflateSync = makeZlibSync("deflate");
	const inflateSync = makeZlibSync("inflate");
	const deflateRawSync = makeZlibSync("deflateRaw");
	const inflateRawSync = makeZlibSync("inflateRaw");
	const unzipSync = makeZlibSync("unzip");
	globalThis.zlib = Object.freeze({
		gzipSync, gunzipSync, deflateSync, inflateSync, deflateRawSync, inflateRawSync, unzipSync,
		gzip: makeZlibAsync(gzipSync),
		gunzip: makeZlibAsync(gunzipSync),
		deflate: makeZlibAsync(deflateSync),
		inflate: makeZlibAsync(inflateSync),
		deflateRaw: makeZlibAsync(deflateRawSync),
		inflateRaw: makeZlibAsync(inflateRawSync),
		unzip: makeZlibAsync(unzipSync),
	});

	// --- 6. util (format, inspect, promisify, stripVTControlCharacters, types, parseArgs) ---
	const ansiRegex = /\x1B(?:[@-Z\\-_]|\[[0-?]*[ -/]*[@-~])/g;
	function inspectVal(v, seen) {
		if (v === null) return "null";
		if (v === undefined) return "undefined";
		const t = typeof v;
		if (t === "string") return JSON.stringify(v);
		if (t === "bigint") return v.toString() + "n";
		if (t !== "object") return String(v);
		if (v instanceof Error) return v.stack || (v.name + ": " + v.message);
		if (v instanceof RegExp || v instanceof Date) return String(v);
		if (v instanceof Buffer) return "<Buffer " + Array.from(v.subarray(0, 32)).map(b => b.toString(16).padStart(2, "0")).join(" ") + (v.length > 32 ? " ... " + (v.length - 32) + " more bytes" : "") + ">";
		seen = seen || new Set();
		if (seen.has(v)) return "[Circular]";
		seen.add(v);
		if (Array.isArray(v)) return "[ " + v.map(x => inspectVal(x, seen)).join(", ") + " ]";
		if (v instanceof Map) return "Map(" + v.size + ") { " + Array.from(v.entries()).map(([k, val]) => inspectVal(k, seen) + " => " + inspectVal(val, seen)).join(", ") + " }";
		if (v instanceof Set) return "Set(" + v.size + ") { " + Array.from(v.values()).map(val => inspectVal(val, seen)).join(", ") + " }";
		const pairs = Object.keys(v).map(k => k + ": " + inspectVal(v[k], seen));
		return pairs.length ? "{ " + pairs.join(", ") + " }" : "{}";
	}
	function utilFormat(...args) {
		if (args.length === 0) return "";
		if (typeof args[0] !== "string") return args.map(a => inspectVal(a)).join(" ");
		let i = 1;
		let out = args[0].replace(/%[sdifjoO%]/g, (m) => {
			if (m === "%%") return "%";
			if (i >= args.length) return m;
			const a = args[i++];
			switch (m) {
				case "%s": return String(a);
				case "%d": case "%f": return String(Number(a));
				case "%i": return String(parseInt(a, 10));
				case "%j": try { return JSON.stringify(a); } catch (_) { return "[Circular]"; }
				default: return inspectVal(a);
			}
		});
		while (i < args.length) {
			const a = args[i++];
			out += " " + (typeof a === "string" ? a : inspectVal(a));
		}
		return out;
	}
	function promisify(orig) {
		if (typeof orig !== "function") throw new TypeError("util.promisify requires a function");
		return function(...args) {
			return new Promise((resolve, reject) => {
				orig.call(this, ...args, (err, val) => (err ? reject(err) : resolve(val)));
			});
		};
	}
	function callbackify(orig) {
		if (typeof orig !== "function") throw new TypeError("util.callbackify requires a function");
		return function(...args) {
			const cb = args.pop();
			Promise.resolve().then(() => orig.apply(this, args)).then((r) => cb(null, r), (e) => cb(e || new Error("Rejected")));
		};
	}
	function parseArgs(cfg = {}) {
		const rawArgs = Array.isArray(cfg.args) ? cfg.args : [];
		const optDefs = cfg.options || {};
		const shortMap = {};
		for (const [k, def] of Object.entries(optDefs)) {
			if (def && def.short) shortMap[def.short] = k;
		}
		const values = {};
		for (const [k, def] of Object.entries(optDefs)) {
			if (def && def.default !== undefined) values[k] = def.default;
		}
		const positionals = [];
		for (let i = 0; i < rawArgs.length; i++) {
			const a = String(rawArgs[i]);
			if (a === "--") { positionals.push(...rawArgs.slice(i + 1).map(String)); break; }
			if (a.startsWith("--")) {
				const eq = a.indexOf("=");
				const name = eq >= 0 ? a.slice(2, eq) : a.slice(2);
				const def = optDefs[name] || { type: eq >= 0 ? "string" : "boolean" };
				const val = def.type === "boolean" ? (eq >= 0 ? a.slice(eq + 1) !== "false" : true) : (eq >= 0 ? a.slice(eq + 1) : String(rawArgs[++i] ?? ""));
				if (def.multiple) { values[name] = values[name] || []; values[name].push(val); } else { values[name] = val; }
			} else if (a.startsWith("-") && a.length > 1) {
				const shortKey = a.slice(1, 2);
				const name = shortMap[shortKey] || shortKey;
				const def = optDefs[name] || { type: "boolean" };
				const val = def.type === "boolean" ? true : (a.length > 2 ? a.slice(2) : String(rawArgs[++i] ?? ""));
				if (def.multiple) { values[name] = values[name] || []; values[name].push(val); } else { values[name] = val; }
			} else {
				positionals.push(a);
			}
		}
		return { values, positionals };
	}
	globalThis.util = Object.freeze({
		format: utilFormat,
		inspect: (v) => inspectVal(v),
		promisify,
		callbackify,
		stripVTControlCharacters: (s) => String(s).replace(ansiRegex, ""),
		parseArgs,
		TextEncoder,
		TextDecoder,
		types: Object.freeze({
			isTypedArray: (v) => ArrayBuffer.isView(v) && !(v instanceof DataView),
			isUint8Array: (v) => v instanceof Uint8Array,
			isBuffer: (v) => Buffer.isBuffer(v),
			isArrayBuffer: (v) => v instanceof ArrayBuffer,
			isDate: (v) => v instanceof Date,
			isRegExp: (v) => v instanceof RegExp,
			isPromise: (v) => (typeof Promise !== "undefined" && v instanceof Promise) || (!!v && typeof v === "object" && typeof v.then === "function"),
			isMap: (v) => v instanceof Map,
			isSet: (v) => v instanceof Set,
			isNativeError: (v) => v instanceof Error,
		}),
	});

	// --- 7. assert & assert/strict ---
	class AssertionError extends Error {
		constructor(opts = {}) {
			super(opts.message || ("Assertion failed: " + inspectVal(opts.actual) + " " + (opts.operator || "!==") + " " + inspectVal(opts.expected)));
			this.name = "AssertionError";
			this.actual = opts.actual;
			this.expected = opts.expected;
			this.operator = opts.operator || "ok";
			this.code = "ERR_ASSERTION";
		}
	}
	function deepEq(a, b, strict, seen) {
		if (Object.is(a, b)) return true;
		if (!strict && a == b) return true;
		if (a === null || b === null || typeof a !== "object" || typeof b !== "object") return false;
		if (a instanceof Date && b instanceof Date) return a.getTime() === b.getTime();
		if (a instanceof RegExp && b instanceof RegExp) return a.source === b.source && a.flags === b.flags;
		if (a instanceof Uint8Array && b instanceof Uint8Array) {
			if (a.byteLength !== b.byteLength) return false;
			for (let i = 0; i < a.byteLength; i++) if (a[i] !== b[i]) return false;
			return true;
		}
		seen = seen || new Map();
		if (seen.get(a) === b) return true;
		seen.set(a, b);
		if (Array.isArray(a) !== Array.isArray(b)) return false;
		if (Array.isArray(a)) {
			if (a.length !== b.length) return false;
			for (let i = 0; i < a.length; i++) if (!deepEq(a[i], b[i], strict, seen)) return false;
			return true;
		}
		if (a instanceof Map && b instanceof Map) {
			if (a.size !== b.size) return false;
			for (const [k, v] of a.entries()) if (!b.has(k) || !deepEq(v, b.get(k), strict, seen)) return false;
			return true;
		}
		if (a instanceof Set && b instanceof Set) {
			if (a.size !== b.size) return false;
			for (const v of a.values()) if (!b.has(v)) return false;
			return true;
		}
		if (strict && Object.getPrototypeOf(a) !== Object.getPrototypeOf(b)) return false;
		const ka = Object.keys(a), kb = Object.keys(b);
		if (ka.length !== kb.length) return false;
		for (const k of ka) {
			if (!Object.prototype.hasOwnProperty.call(b, k) || !deepEq(a[k], b[k], strict, seen)) return false;
		}
		return true;
	}
	function checkErrMatch(err, matcher) {
		if (!matcher) return true;
		if (matcher instanceof RegExp) return matcher.test(String(err && (err.message || err)));
		if (typeof matcher === "function") {
			if (matcher.prototype instanceof Error || matcher === Error) return err instanceof matcher;
			return Boolean(matcher(err));
		}
		if (typeof matcher === "object") {
			for (const k of Object.keys(matcher)) {
				const exp = matcher[k];
				const got = err ? err[k] : undefined;
				if (exp instanceof RegExp ? !exp.test(String(got)) : got !== exp) return false;
			}
			return true;
		}
		return true;
	}
	function assertFn(value, message) {
		if (!value) throw new AssertionError({ actual: value, expected: true, operator: "==", message: message ? String(message) : undefined });
	}
	assertFn.AssertionError = AssertionError;
	assertFn.ok = assertFn;
	assertFn.fail = (msg) => { throw new AssertionError({ message: msg ? String(msg) : "Failed", operator: "fail" }); };
	assertFn.ifError = (err) => { if (err !== null && err !== undefined) throw err; };
	assertFn.equal = (a, b, m) => { if (!(a == b)) throw new AssertionError({ actual: a, expected: b, operator: "==", message: m }); };
	assertFn.notEqual = (a, b, m) => { if (a == b) throw new AssertionError({ actual: a, expected: b, operator: "!=", message: m }); };
	assertFn.strictEqual = (a, b, m) => { if (!Object.is(a, b)) throw new AssertionError({ actual: a, expected: b, operator: "strictEqual", message: m }); };
	assertFn.notStrictEqual = (a, b, m) => { if (Object.is(a, b)) throw new AssertionError({ actual: a, expected: b, operator: "notStrictEqual", message: m }); };
	assertFn.deepEqual = (a, b, m) => { if (!deepEq(a, b, false)) throw new AssertionError({ actual: a, expected: b, operator: "deepEqual", message: m }); };
	assertFn.notDeepEqual = (a, b, m) => { if (deepEq(a, b, false)) throw new AssertionError({ actual: a, expected: b, operator: "notDeepEqual", message: m }); };
	assertFn.deepStrictEqual = (a, b, m) => { if (!deepEq(a, b, true)) throw new AssertionError({ actual: a, expected: b, operator: "deepStrictEqual", message: m }); };
	assertFn.notDeepStrictEqual = (a, b, m) => { if (deepEq(a, b, true)) throw new AssertionError({ actual: a, expected: b, operator: "notDeepStrictEqual", message: m }); };
	assertFn.match = (str, re, m) => { if (!re.test(String(str))) throw new AssertionError({ actual: str, expected: re, operator: "match", message: m }); };
	assertFn.doesNotMatch = (str, re, m) => { if (re.test(String(str))) throw new AssertionError({ actual: str, expected: re, operator: "doesNotMatch", message: m }); };
	assertFn.throws = (fn, errMatcher, msg) => {
		const m = typeof errMatcher === "string" ? errMatcher : msg;
		const matcher = typeof errMatcher === "string" ? null : errMatcher;
		let caught = false, thrown;
		try { fn(); } catch (e) { caught = true; thrown = e; }
		if (!caught) throw new AssertionError({ message: m || "Missing expected exception", operator: "throws" });
		if (!checkErrMatch(thrown, matcher)) throw new AssertionError({ actual: thrown, expected: matcher, operator: "throws", message: m });
	};
	assertFn.doesNotThrow = (fn, msg) => {
		try { fn(); } catch (e) { throw new AssertionError({ actual: e, operator: "doesNotThrow", message: msg || ("Got unwanted exception: " + String(e)) }); }
	};
	assertFn.rejects = async (asyncFnOrPromise, errMatcher, msg) => {
		const m = typeof errMatcher === "string" ? errMatcher : msg;
		const matcher = typeof errMatcher === "string" ? null : errMatcher;
		let caught = false, thrown;
		try { await (typeof asyncFnOrPromise === "function" ? asyncFnOrPromise() : asyncFnOrPromise); } catch (e) { caught = true; thrown = e; }
		if (!caught) throw new AssertionError({ message: m || "Missing expected rejection", operator: "rejects" });
		if (!checkErrMatch(thrown, matcher)) throw new AssertionError({ actual: thrown, expected: matcher, operator: "rejects", message: m });
	};
	assertFn.doesNotReject = async (asyncFnOrPromise, msg) => {
		try { await (typeof asyncFnOrPromise === "function" ? asyncFnOrPromise() : asyncFnOrPromise); } catch (e) {
			throw new AssertionError({ actual: e, operator: "doesNotReject", message: msg || ("Got unwanted rejection: " + String(e)) });
		}
	};
	assertFn.strict = assertFn;
	globalThis.assert = assertFn;

	// --- 8. events (EventEmitter), querystring, string_decoder ---
	class EventEmitter {
		constructor() { this._events = Object.create(null); }
		on(ev, fn) { (this._events[ev] = this._events[ev] || []).push(fn); return this; }
		addListener(ev, fn) { return this.on(ev, fn); }
		once(ev, fn) {
			const wrap = (...args) => { this.off(ev, wrap); fn.apply(this, args); };
			wrap._orig = fn;
			return this.on(ev, wrap);
		}
		off(ev, fn) {
			const list = this._events[ev];
			if (list) this._events[ev] = list.filter(f => f !== fn && f._orig !== fn);
			return this;
		}
		removeListener(ev, fn) { return this.off(ev, fn); }
		removeAllListeners(ev) {
			if (ev === undefined) this._events = Object.create(null);
			else delete this._events[ev];
			return this;
		}
		emit(ev, ...args) {
			const list = this._events[ev];
			if (!list || list.length === 0) {
				if (ev === "error") throw (args[0] instanceof Error ? args[0] : new Error(String(args[0] || "Unhandled error event")));
				return false;
			}
			for (const fn of list.slice()) fn.apply(this, args);
			return true;
		}
		listenerCount(ev) { return (this._events[ev] || []).length; }
		listeners(ev) { return (this._events[ev] || []).map(f => f._orig || f); }
		eventNames() { return Object.keys(this._events).filter(k => this._events[k] && this._events[k].length > 0); }
	}
	EventEmitter.EventEmitter = EventEmitter;
	EventEmitter.once = (emitter, name) => new Promise((resolve, reject) => {
		const onErr = (err) => { emitter.off(name, onVal); reject(err); };
		const onVal = (...args) => { if (name !== "error") emitter.off("error", onErr); resolve(args); };
		emitter.once(name, onVal);
		if (name !== "error") emitter.once("error", onErr);
	});
	globalThis.EventEmitter = EventEmitter;
	globalThis.events = EventEmitter;

	const qsParse = (str) => {
		const out = Object.create(null);
		const sp = new URLSearchParams(String(str || ""));
		for (const [k, v] of sp) {
			if (k in out) {
				if (Array.isArray(out[k])) out[k].push(v);
				else out[k] = [out[k], v];
			} else {
				out[k] = v;
			}
		}
		return out;
	};
	const qsStringify = (obj) => {
		if (!obj || typeof obj !== "object") return "";
		const sp = new URLSearchParams();
		for (const k of Object.keys(obj)) {
			const v = obj[k];
			if (Array.isArray(v)) { for (const item of v) sp.append(k, item); }
			else if (v !== undefined) sp.append(k, v === null ? "" : v);
		}
		return sp.toString();
	};
	globalThis.querystring = Object.freeze({
		parse: qsParse, decode: qsParse,
		stringify: qsStringify, encode: qsStringify,
		escape: encodeURIComponent, unescape: decodeURIComponent,
	});

	class StringDecoder {
		constructor(encoding = "utf8") { this.encoding = encoding; }
		write(buf) { return Buffer.isBuffer(buf) ? buf.toString(this.encoding) : Buffer.from(buf).toString(this.encoding); }
		end(buf) { return buf ? this.write(buf) : ""; }
	}
	globalThis.StringDecoder = StringDecoder;
	globalThis.string_decoder = Object.freeze({ StringDecoder });
})()`, false)

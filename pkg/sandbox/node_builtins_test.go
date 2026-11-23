package sandbox

import (
	"context"
	"strings"
	"testing"
	"time"
)

func TestNodeBuiltinsSuite(t *testing.T) {
	ws := t.TempDir()
	res, err := Execute(context.Background(), ExecRequest{
		Cwd:     ws,
		Timeout: 10 * time.Second,
		Code: `
import cryptoMod from "node:crypto";
import * as zlibMod from "node:zlib";
import { promisify, format as fmt, stripVTControlCharacters, parseArgs } from "node:util";
import assertStrict from "node:assert/strict";
import { EventEmitter, once } from "node:events";
import posix from "node:path/posix";
import win32 from "node:path/win32";
import { setTimeout as delay } from "node:timers/promises";

// 1. Buffer DataView integer/BigInt/float reads & writes + subarray offset safety + search + swap
const b = Buffer.alloc(32);
b.writeUInt8(0xfe, 0);
b.writeInt8(-12, 1);
b.writeUInt16LE(0x1234, 2);
b.writeUInt16BE(0x1234, 4);
b.writeInt32LE(-12345678, 6);
b.writeUInt32BE(0xdeadbeef, 10);
b.writeBigUInt64LE(BigInt("0xfedcba9876543210"), 16);
b.writeBigInt64BE(BigInt("-9007199254740993"), 24);

assertStrict.strictEqual(b.readUInt8(0), 0xfe);
assertStrict.strictEqual(b.readInt8(1), -12);
assertStrict.strictEqual(b.readUInt16LE(2), 0x1234);
assertStrict.strictEqual(b.readUInt16BE(4), 0x1234);
assertStrict.strictEqual(b.readInt32LE(6), -12345678);
assertStrict.strictEqual(b.readUInt32BE(10), 0xdeadbeef);
assertStrict.strictEqual(b.readBigUInt64LE(16).toString(16), "fedcba9876543210");
assertStrict.strictEqual(b.readBigInt64BE(24).toString(), "-9007199254740993");

// Subarray view must respect non-zero byteOffset!
const sub = b.subarray(16, 24);
assertStrict.strictEqual(sub.readBigUInt64LE(0).toString(16), "fedcba9876543210");
assertStrict.throws(() => sub.readUInt32LE(6), RangeError);

// 48-bit readUIntLE/BE + float/double + indexOf/includes + swap32
const b48 = Buffer.alloc(16);
b48.writeUIntLE(0x123456789abc, 0, 6);
b48.writeIntBE(-0x123456789a, 6, 6);
b48.writeDoubleLE(3.141592653589793, 8);
assertStrict.strictEqual(b48.readUIntLE(0, 6), 0x123456789abc);
assertStrict.strictEqual(b48.readDoubleLE(8), 3.141592653589793);

const magic = Buffer.from("\x7fELF_payload_ELF", "latin1");
assertStrict.strictEqual(magic.indexOf("ELF"), 1);
assertStrict.strictEqual(magic.lastIndexOf("ELF"), 13);
assertStrict.strictEqual(magic.includes(Buffer.from("payload")), true);
const sw = Buffer.from([1, 2, 3, 4]).swap32();
assertStrict.deepStrictEqual(Array.from(sw), [4, 3, 2, 1]);

// 2. path.relative & path.format (posix & win32)
assertStrict.strictEqual(posix.relative("/data/orandea/test/aaa", "/data/orandea/impl/bbb"), "../../impl/bbb");
assertStrict.strictEqual(win32.relative("C:\\orandea\\test\\aaa", "c:\\orandea\\impl\\bbb"), "..\\..\\impl\\bbb");
assertStrict.strictEqual(posix.format({ dir: "/home/user", name: "app", ext: "ts" }), "/home/user/app.ts");

// 3. crypto HMAC, one-shot hash, copy(), timingSafeEqual, randomInt, randomFillSync
const hmacHex = cryptoMod.createHmac("sha256", "key").update("The quick brown fox").digest("hex");
assertStrict.strictEqual(hmacHex.length, 64);
const h1 = crypto.createHash("sha256").update("hello ");
const h2 = h1.copy();
assertStrict.strictEqual(h1.update("world").digest("hex"), crypto.hash("sha256", "hello world", "hex"));
assertStrict.strictEqual(h2.update("world").digest("hex"), h1.digest("hex"));
assertStrict.strictEqual(crypto.timingSafeEqual(Buffer.from("abcd"), Buffer.from("abcd")), true);
assertStrict.throws(() => crypto.timingSafeEqual(Buffer.from("a"), Buffer.from("ab")), RangeError);
const ri = crypto.randomInt(10, 20);
assertStrict.ok(ri >= 10 && ri < 20);

// 4. zlib sync + async + promisify + maxOutputLength bomb guard
const payload = Buffer.from("hello-zlib-compression-test-".repeat(50));
const gz = zlibMod.gzipSync(payload);
assertStrict.ok(gz.length < payload.length);
assertStrict.strictEqual(zlibMod.gunzipSync(gz).equals(payload), true);
assertStrict.strictEqual(zlibMod.unzipSync(zlibMod.deflateSync(payload)).equals(payload), true);
assertStrict.strictEqual(zlibMod.inflateRawSync(zlibMod.deflateRawSync(payload)).equals(payload), true);
const asyncGunzip = promisify(zlibMod.gunzip);
assertStrict.strictEqual((await asyncGunzip(gz)).equals(payload), true);
assertStrict.throws(() => zlibMod.gunzipSync(gz, { maxOutputLength: 32 }), /exceeds maxOutputLength/);

// 5. util, assert.rejects, EventEmitter, querystring, StringDecoder
assertStrict.strictEqual(fmt("%s:%d", "port", 8080), "port:8080");
assertStrict.strictEqual(stripVTControlCharacters("\x1b[32mPASS\x1b[0m"), "PASS");
const parsedCli = parseArgs({ args: ["-v", "--out=dist", "main.ts"], options: { v: { type: "boolean" }, out: { type: "string" } } });
assertStrict.deepStrictEqual(parsedCli, { values: { v: true, out: "dist" }, positionals: ["main.ts"] });
await assertStrict.rejects(async () => { throw new TypeError("boom"); }, TypeError);

const ee = new EventEmitter();
setImmediate(() => ee.emit("ready", 77));
const [evVal] = await once(ee, "ready");
assertStrict.strictEqual(evVal, 77);
assertStrict.deepStrictEqual(Object.assign({}, querystring.parse("a=1&b=two&a=3")), { a: ["1", "3"], b: "two" });

// 6. process, os, performance, URL, setInterval (incl. un-cleared background interval cleanup!), structuredClone Buffer
const t0 = performance.now();
const hr0 = process.hrtime.bigint();
await delay(5);
assertStrict.ok(performance.now() >= t0);
assertStrict.ok(process.hrtime.bigint() > hr0);
process.stdout.write("stdout-line\n");
assertStrict.ok(os.cpus().length >= 1 && os.availableParallelism() >= 1);
assertStrict.strictEqual(URL.canParse("https://example.com/x"), true);
assertStrict.strictEqual(URL.canParse("not a url"), false);

let ticks = 0;
await new Promise<void>((resolve) => {
  const iv = setInterval(() => {
    if (++ticks === 3) {
      clearInterval(iv);
      resolve();
    }
  }, 2);
});
assertStrict.strictEqual(ticks, 3);

// Un-cleared setInterval must NOT hang Execute() once top-level async function returns!
setInterval(() => {}, 50);

const clonedBuf = structuredClone(Buffer.from([9, 8, 7]));
assertStrict.strictEqual(Buffer.isBuffer(clonedBuf), true);

return { ok: true, big64: b.readBigUInt64LE(16) };
`,
	})
	if err != nil {
		t.Fatalf("TestNodeBuiltinsSuite failed: %v", err)
	}
	if len(res.Logs) == 0 || !strings.Contains(res.Logs[0], "stdout-line") {
		t.Fatalf("expected process.stdout.write in Logs, got: %+v", res.Logs)
	}
}

package sandbox

import (
	"os"
	"runtime"
	"strings"
)

// installProcessOS adds read-only Node-style `process` (platform, arch, env, cwd()) and `os`
// (platform(), arch(), homedir(), tmpdir(), EOL) globals so programs read env vars and paths without
// spawning a shell. `process.env` is a frozen snapshot (case-insensitive on Windows, like Node);
// pass `{ env }` to sh.exec / sh.run to change a child's environment.
func installProcessOS(env *ExecutionEnv) error {
	vm := env.VM
	vars := map[string]any{}
	for _, kv := range os.Environ() {
		if k, v, ok := strings.Cut(kv, "="); ok && k != "" {
			if isSecretEnvName(k) {
				v = redacted
			}
			vars[k] = v
		}
	}
	home, _ := os.UserHomeDir()
	eol := "\n"
	if runtime.GOOS == "windows" {
		eol = "\r\n"
	}
	info := map[string]any{
		"platform": nodePlatform(),
		"arch":     nodeArch(),
		"env":      vars,
		"cwd":      env.Cwd,
		"home":     home,
		"tmp":      os.TempDir(),
		"eol":      eol,
	}
	if err := vm.Set("__ptcHostInfo", info); err != nil {
		return err
	}
	_, err := vm.RunString(`(function () {
		const h = globalThis.__ptcHostInfo;
		delete globalThis.__ptcHostInfo;
		let envObj = Object.freeze(Object.assign({}, h.env));
		if (h.platform === "win32") {
			const upper = {};
			for (const k of Object.keys(envObj)) upper[k.toUpperCase()] = envObj[k];
			envObj = new Proxy(envObj, {
				get(t, p) { return typeof p === "string" && !(p in t) ? upper[p.toUpperCase()] : t[p]; },
				has(t, p) { return typeof p === "string" ? (p in t) || (p.toUpperCase() in upper) : (p in t); },
			});
		}
		globalThis.process = Object.freeze({
			platform: h.platform, arch: h.arch, env: envObj, cwd: () => h.cwd,
		});
		globalThis.os = Object.freeze({
			platform: () => h.platform, arch: () => h.arch, homedir: () => h.home, tmpdir: () => h.tmp, EOL: h.eol,
		});
	})()`)
	return err
}

// redacted replaces secret-looking env values so `return process.env` never leaks tokens into the
// conversation or audit logs. Child processes (sh.exec / sh.run) still inherit the real values.
const redacted = "[redacted]"

var secretEnvSubstrings = []string{"TOKEN", "SECRET", "PASSWORD", "PASSWD", "PASSPHRASE", "CREDENTIAL", "APIKEY", "API_KEY",
	"ACCESS_KEY", "PRIVATE_KEY", "SECRET_KEY", "COOKIE", "SESSION_KEY", "CONNECTION_STRING"}

var secretEnvSegments = map[string]bool{"KEY": true, "KEYS": true, "AUTH": true, "PAT": true, "PWD": false, "DSN": true}

func isSecretEnvName(name string) bool {
	up := strings.ToUpper(name)
	for _, s := range secretEnvSubstrings {
		if strings.Contains(up, s) {
			return true
		}
	}
	for _, seg := range strings.FieldsFunc(up, func(r rune) bool { return r == '_' || r == '-' || r == '.' }) {
		if secretEnvSegments[seg] {
			return true
		}
	}
	return false
}

func nodePlatform() string {
	if runtime.GOOS == "windows" {
		return "win32"
	}
	return runtime.GOOS
}

func nodeArch() string {
	switch runtime.GOARCH {
	case "amd64":
		return "x64"
	case "386":
		return "ia32"
	}
	return runtime.GOARCH
}

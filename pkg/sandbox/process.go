package sandbox

import (
	"net/url"
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
			} else if strings.Contains(v, "://") && strings.Contains(v, "@") {
				v = redactURLCredentials(v)
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
				get(t, p) {
					if (typeof p !== "string" || p in t || p === "toJSON" || p === "then" || p in Object.prototype) {
						return t[p];
					}
					return upper[p.toUpperCase()];
				},
				has(t, p) {
					return typeof p === "string" ? (p in t) || (p.toUpperCase() in upper) : (p in t);
				},
				ownKeys(t) {
					return Reflect.ownKeys(t);
				},
				getOwnPropertyDescriptor(t, p) {
					if (typeof p === "string" && !(p in t) && (p.toUpperCase() in upper)) {
						return { value: upper[p.toUpperCase()], enumerable: true, configurable: true, writable: false };
					}
					return Object.getOwnPropertyDescriptor(t, p);
				},
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

var secretEnvSubstrings = []string{
	"TOKEN", "SECRET", "PASSWORD", "PASSWD", "PASSPHRASE", "CREDENTIAL", "APIKEY", "API_KEY",
	"ACCESS_KEY", "PRIVATE_KEY", "SECRET_KEY", "COOKIE", "SESSION", "SESSION_KEY",
	"AUTHORIZATION", "BEARER", "CONNECTION_STRING",
}

var secretEnvSegments = map[string]bool{
	"KEY": true, "KEYS": true, "AUTH": true, "AUTHORIZATION": true, "BEARER": true,
	"COOKIE": true, "SESSION": true, "PAT": true, "PWD": false, "DSN": true,
}

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

// redactURLCredentials scrubs userinfo (`user:pass@host` -> `user:redacted@host` and single-token
// `token@host` -> `redacted@host`), including URLs where `url.Parse` fails or misparses authority
// due to unescaped `#`, `?`, or `%` characters inside passwords.
func redactURLCredentials(val string) string {
	if !strings.Contains(val, "://") || !strings.Contains(val, "@") {
		return val
	}
	// Fast path when `val` is a single standard URL with clean `u.User` and no `#`/`?` before `@`.
	if !strings.ContainsAny(val, " \t\r\n,;\"'") {
		if redactedSingle, handled := redactSingleURLToken(val); handled {
			return redactedSingle
		}
	}
	// General scanner over any `scheme://...` substrings inside `val`.
	var b strings.Builder
	b.Grow(len(val))
	i := 0
	for i < len(val) {
		idx := strings.Index(val[i:], "://")
		if idx < 0 {
			b.WriteString(val[i:])
			break
		}
		colonPos := i + idx
		schemeStart := colonPos
		for schemeStart > i && isURLSchemeChar(val[schemeStart-1]) {
			schemeStart--
		}
		if schemeStart == colonPos || !isASCIILetter(val[schemeStart]) {
			b.WriteString(val[i : colonPos+3])
			i = colonPos + 3
			continue
		}
		b.WriteString(val[i:schemeStart])
		tokEnd := colonPos + 3
		for tokEnd < len(val) && !isURLTokenStop(val[tokEnd]) {
			tokEnd++
		}
		tok := val[schemeStart:tokEnd]
		redTok, _ := redactSingleURLToken(tok)
		b.WriteString(redTok)
		i = tokEnd
	}
	return b.String()
}

func redactSingleURLToken(tok string) (string, bool) {
	schemeSep := strings.Index(tok, "://")
	if schemeSep <= 0 {
		return tok, false
	}
	afterScheme := tok[schemeSep+3:]
	slashIdx := strings.IndexByte(afterScheme, '/')
	authorityCand := afterScheme
	suffix := ""
	if slashIdx >= 0 {
		authorityCand = afterScheme[:slashIdx]
		suffix = afterScheme[slashIdx:]
	}
	atIdx := strings.LastIndexByte(authorityCand, '@')
	if atIdx < 0 {
		// `@` only appears after `/` (e.g. `https://example.com/path?email=a@b.com`) -> no credentials.
		return tok, true
	}
	userInfo := authorityCand[:atIdx]
	hostPart := authorityCand[atIdx+1:]
	if hostPart == "" {
		return tok, true
	}

	// Guard slashless URLs with `@` in query/fragment (`https://example.com?email=a@b.com`):
	// if `userInfo` contains `?` or `#`, check whether everything before `?`/`#` is already a valid `host[:port]`
	// followed by a `key=val` query rather than `user:pass#...@host`.
	if slashIdx < 0 {
		if qIdx := strings.IndexAny(userInfo, "?#"); qIdx >= 0 {
			preQ := userInfo[:qIdx]
			if isCleanHostPort(preQ) && strings.Contains(userInfo[qIdx+1:], "=") {
				return tok, true
			}
		}
	}

	// If standard `url.Parse` succeeds without `#`/`?`/`%` ambiguity in `userInfo`, check consistency.
	if !strings.ContainsAny(userInfo, "#?%") {
		if u, err := url.Parse(tok); err == nil && u.User != nil {
			if _, hasPass := u.User.Password(); hasPass {
				u.User = url.UserPassword(u.User.Username(), "redacted")
			} else if u.User.Username() != "" {
				u.User = url.User("redacted")
			}
			return u.String(), true
		}
	}

	// Direct authority rewrite for single-token userinfo or passwords containing unescaped `#`, `?`, `%`, or `@`.
	var scrubbedUser string
	if colonIdx := strings.IndexByte(userInfo, ':'); colonIdx >= 0 {
		user := userInfo[:colonIdx]
		scrubbedUser = user + ":redacted"
	} else if userInfo != "" {
		scrubbedUser = "redacted"
	}
	return tok[:schemeSep+3] + scrubbedUser + "@" + hostPart + suffix, true
}

func isCleanHostPort(s string) bool {
	if s == "" {
		return false
	}
	colon := strings.IndexByte(s, ':')
	host := s
	if colon >= 0 {
		host = s[:colon]
		port := s[colon+1:]
		if len(port) == 0 || len(port) > 5 {
			return false
		}
		for i := 0; i < len(port); i++ {
			if port[i] < '0' || port[i] > '9' {
				return false
			}
		}
	}
	return strings.Contains(host, ".") || strings.EqualFold(host, "localhost")
}

func isASCIILetter(c byte) bool {
	return (c >= 'a' && c <= 'z') || (c >= 'A' && c <= 'Z')
}

func isURLSchemeChar(c byte) bool {
	return isASCIILetter(c) || (c >= '0' && c <= '9') || c == '+' || c == '-' || c == '.'
}

func isURLTokenStop(c byte) bool {
	switch c {
	case ' ', '\t', '\r', '\n', ',', ';', '"', '\'':
		return true
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

package oauthserver

import (
	"errors"
	"fmt"
	"net"
	"net/url"
	"strings"
	"sync"
	"time"
)

var errOwnerSecretRequired = errors.New("OAuth requires an owner secret (--token) to approve clients on the consent page")

// redirectRule is one allowed redirect URI pattern.
type redirectRule struct {
	loopback bool   // http://127.0.0.1|localhost|[::1]:<any port>/<any path>
	prefix   string // pattern ending in "*": prefix match
	exact    string
}

func parseRedirectRules(extra []string) ([]redirectRule, error) {
	var rules []redirectRule
	add := func(p string) error {
		p = strings.TrimSpace(p)
		if p == "" {
			return nil
		}
		if strings.EqualFold(p, "loopback") {
			rules = append(rules, redirectRule{loopback: true})
			return nil
		}
		base := strings.TrimSuffix(p, "*")
		u, err := url.Parse(base)
		if err != nil || u.Host == "" || (u.Scheme != "https" && u.Scheme != "http") {
			return fmt.Errorf("invalid --oauth-redirect-allow pattern %q (use an absolute http(s) URL, optionally ending in *, or \"loopback\")", p)
		}
		if strings.HasSuffix(p, "*") {
			// The wildcard must follow a path separator so the host part is fully pinned
			// (e.g. "https://a.example/r/*", never "https://a.example*").
			if u.Path == "" || !strings.HasSuffix(base, "/") {
				return fmt.Errorf("invalid --oauth-redirect-allow pattern %q: a wildcard must come after a path ending in /", p)
			}
			rules = append(rules, redirectRule{prefix: base})
		} else {
			rules = append(rules, redirectRule{exact: p})
		}
		return nil
	}
	for _, p := range GoogleRedirectPatterns {
		if err := add(p); err != nil {
			return nil, err
		}
	}
	for _, p := range extra {
		if err := add(p); err != nil {
			return nil, err
		}
	}
	return rules, nil
}

// redirectAllowed reports whether uri may be registered / used as a redirect URI.
func (s *Server) redirectAllowed(uri string) bool {
	if uri == "" || strings.ContainsAny(uri, " \t\r\n\\") {
		return false
	}
	u, err := url.Parse(uri)
	if err != nil || !u.IsAbs() || u.Host == "" || u.Fragment != "" || u.User != nil || strings.Contains(uri, "#") {
		return false
	}
	for _, rule := range s.rules {
		switch {
		case rule.loopback:
			h := u.Hostname()
			ip := net.ParseIP(h)
			if u.Scheme == "http" && (strings.EqualFold(h, "localhost") || (ip != nil && ip.IsLoopback())) {
				return true
			}
		case rule.prefix != "":
			if strings.HasPrefix(uri, rule.prefix) && !strings.Contains(uri[len(rule.prefix):], "..") {
				return true
			}
		case rule.exact != "":
			if uri == rule.exact {
				return true
			}
		}
	}
	return false
}

// failureLimiter locks out a caller after repeated wrong owner secrets, with a global backstop so distributed
// guessing cannot brute-force the secret either.
type failureLimiter struct {
	mu          sync.Mutex
	fails       map[string][]time.Time
	lockedUntil map[string]time.Time
	global      []time.Time
	globalLock  time.Time
}

const (
	perKeyMaxFails = 5
	perKeyWindow   = 15 * time.Minute
	globalMaxFails = 30
	globalWindow   = time.Hour
)

func newFailureLimiter() *failureLimiter {
	return &failureLimiter{fails: map[string][]time.Time{}, lockedUntil: map[string]time.Time{}}
}

// locked reports whether key may not attempt a login now, and for how long.
func (l *failureLimiter) locked(key string, now time.Time) (bool, time.Duration) {
	l.mu.Lock()
	defer l.mu.Unlock()
	if now.Before(l.globalLock) {
		return true, l.globalLock.Sub(now)
	}
	if until := l.lockedUntil[key]; now.Before(until) {
		return true, until.Sub(now)
	}
	return false, 0
}

func (l *failureLimiter) fail(key string, now time.Time) {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.fails[key] = pruneTimes(append(l.fails[key], now), now, perKeyWindow)
	if len(l.fails[key]) >= perKeyMaxFails {
		l.lockedUntil[key] = now.Add(perKeyWindow)
		delete(l.fails, key)
	}
	l.global = pruneTimes(append(l.global, now), now, globalWindow)
	if len(l.global) >= globalMaxFails {
		l.globalLock = now.Add(globalWindow)
		l.global = nil
	}
	if len(l.fails) > 4096 {
		l.fails = map[string][]time.Time{}
	}
	if len(l.lockedUntil) > 4096 {
		for k, until := range l.lockedUntil {
			if now.After(until) {
				delete(l.lockedUntil, k)
			}
		}
	}
}

func (l *failureLimiter) success(key string) {
	l.mu.Lock()
	delete(l.fails, key)
	l.mu.Unlock()
}

func pruneTimes(ts []time.Time, now time.Time, window time.Duration) []time.Time {
	out := ts[:0]
	for _, t := range ts {
		if now.Sub(t) < window {
			out = append(out, t)
		}
	}
	return out
}

// rateCounter allows at most max events per key per window.
type rateCounter struct {
	mu     sync.Mutex
	max    int
	window time.Duration
	events map[string][]time.Time
}

func newRateCounter(max int, window time.Duration) *rateCounter {
	return &rateCounter{max: max, window: window, events: map[string][]time.Time{}}
}

func (c *rateCounter) allow(key string, now time.Time) bool {
	c.mu.Lock()
	defer c.mu.Unlock()
	ev := pruneTimes(c.events[key], now, c.window)
	if len(ev) >= c.max {
		c.events[key] = ev
		return false
	}
	c.events[key] = append(ev, now)
	if len(c.events) > 4096 {
		c.events = map[string][]time.Time{key: c.events[key]}
	}
	return true
}

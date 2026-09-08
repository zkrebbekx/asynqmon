// Package loglimit collapses a repeating identical error into one log line
// per minute.
//
// A background loop that fails for a structural reason fails on every tick.
// A Redis ACL user without write permission, for example, never acquires a
// role lease: three roles at a 5-second tick print about 51,000 identical
// lines a day. The Limiter keeps the signal and drops the flood. It logs the
// FIRST occurrence of a message at once, then at most one line per minute
// while the same message repeats, then one recovery line when the same key
// succeeds again.
//
// The Limiter lives in internal/ because three packages need it: stats,
// errsig and hygiene. errsig must not import stats for a logging helper.
//
// A caller picks one of two entry points:
//
//   - Message returns a ready-made line ("<key> failed: <err>").
//   - Report calls the caller's logger and keeps the caller's own wording of
//     the first line, so an existing grep still matches.
//
// The clock is injectable, so a test steps over the interval instead of
// sleeping.
package loglimit

import (
	"fmt"
	"sync"
	"time"
)

// Interval is how often a repeating identical error is logged again.
const Interval = time.Minute

// Limiter holds the per-key failure state. The zero value is not usable;
// call New. A Limiter is safe for concurrent use.
type Limiter struct {
	mu sync.Mutex
	// state per key: the last message logged and when.
	last    map[string]string
	lastLog map[string]time.Time
	count   map[string]int
	clock   func() time.Time
}

// New creates a Limiter. A nil clock means time.Now.
func New(clock func() time.Time) *Limiter {
	if clock == nil {
		clock = time.Now
	}
	return &Limiter{
		last:    make(map[string]string),
		lastLog: make(map[string]time.Time),
		count:   make(map[string]int),
		clock:   clock,
	}
}

// Outcome reports what the caller must log for one observation. Exactly one
// of First, Repeat and Recovered is true when Log is true.
type Outcome struct {
	// Log is true when the caller must write one line.
	Log bool
	// First is true for the first failure of a new message.
	First bool
	// Repeat is true for the once-per-Interval line while the same message
	// repeats.
	Repeat bool
	// Recovered is true for the single line after the key succeeds again.
	Recovered bool
	// Count is the number of failures seen for the key, including this one.
	// On a Recovered outcome it counts the failures before the recovery.
	Count int
	// LastErr is the message of the last failure. It is empty on First.
	LastErr string
}

// Observe records one outcome of key and reports what to log. Pass err=nil
// for a success. Each key carries its own state, so two failing operations
// never mask each other.
func (l *Limiter) Observe(key string, err error) Outcome {
	l.mu.Lock()
	defer l.mu.Unlock()
	now := l.clock()
	if err == nil {
		prev, had := l.last[key]
		if !had {
			return Outcome{}
		}
		n := l.count[key]
		delete(l.last, key)
		delete(l.lastLog, key)
		delete(l.count, key)
		return Outcome{Log: true, Recovered: true, Count: n, LastErr: prev}
	}
	msg := err.Error()
	l.count[key]++
	if prev, had := l.last[key]; had && prev == msg {
		if now.Sub(l.lastLog[key]) < Interval {
			return Outcome{Count: l.count[key], LastErr: msg}
		}
		l.lastLog[key] = now
		return Outcome{Log: true, Repeat: true, Count: l.count[key], LastErr: msg}
	}
	l.last[key] = msg
	l.lastLog[key] = now
	l.count[key] = 1
	return Outcome{Log: true, First: true, Count: 1}
}

// Message records one outcome of key and returns the line to log. The second
// result is false when the caller must say nothing: either the key is healthy
// and was healthy before, or the identical error was logged less than
// Interval ago.
func (l *Limiter) Message(key string, err error) (string, bool) {
	out := l.Observe(key, err)
	switch {
	case !out.Log:
		return "", false
	case out.Recovered:
		return fmt.Sprintf("%s recovered after %d failures (last: %s)", key, out.Count, out.LastErr), true
	case out.Repeat:
		return fmt.Sprintf("%s failing: %s (%d times)", key, out.LastErr, out.Count), true
	default:
		return fmt.Sprintf("%s failed: %s", key, err.Error()), true
	}
}

// Report records one outcome of key and writes at most one line through logf.
//
// prefix is the caller's own description of the operation, without the error
// and without a trailing colon, for example
// "asynqmon: stats: acquiring sweeper lease". The first line is exactly
// "<prefix>: <err>", so an operator who greps for the old text still finds
// it. A repeat line adds the count; the recovery line reports the number of
// failures that ended.
func (l *Limiter) Report(logf func(format string, args ...interface{}), key, prefix string, err error) {
	out := l.Observe(key, err)
	if !out.Log {
		return
	}
	switch {
	case out.Recovered:
		logf("%s: recovered after %d failures (last: %s)", prefix, out.Count, out.LastErr)
	case out.Repeat:
		logf("%s: %v (%d times in the last %s)", prefix, err, out.Count, Interval)
	default:
		logf("%s: %v", prefix, err)
	}
}

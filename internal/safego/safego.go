// Package safego runs work in a goroutine that survives a panic.
//
// net/http recovers a panic only on the goroutine that serves the request.
// A panic on any goroutine that asynqmon starts itself — a handler fan-out
// worker, a background seeder, a lease loop — ends the whole process. Every
// such goroutine goes through this package, so a panic costs one unit of
// work and one log line instead of the dashboard.
//
// The three entry points differ in who owns the goroutine:
//
//   - Run runs fn on the calling goroutine and reports whether fn panicked.
//   - Go runs fn on a new goroutine.
//   - Loop runs fn on the calling goroutine and restarts fn after a panic.
//   - GoLoop runs Loop on a new goroutine.
//
// Recovery hides a bug, so every recovered panic writes the panic value and
// the stack to the logger. Use SetLogger to capture that output in a test.
package safego

import (
	"context"
	"log"
	"runtime/debug"
	"sync"
	"time"
)

// Backoff schedule that Loop uses between restarts.
const (
	// loopBackoffInitial is the pause after the first panic.
	loopBackoffInitial = 100 * time.Millisecond
	// loopBackoffMax caps the pause between restarts.
	loopBackoffMax = 5 * time.Second
	// loopHealthyRun is the run time after which Loop treats the previous
	// run as healthy and restarts from loopBackoffInitial again. Without it
	// one panic a day would leave the loop at the maximum pause forever.
	loopHealthyRun = time.Minute
)

var (
	logMu sync.RWMutex
	logfn = log.Printf
)

// SetLogger installs the function that reports a recovered panic. Pass nil
// to restore log.Printf. A test uses it to capture the panic report.
func SetLogger(fn func(format string, v ...any)) {
	logMu.Lock()
	defer logMu.Unlock()
	if fn == nil {
		fn = log.Printf
	}
	logfn = fn
}

// logf writes one line through the installed logger.
func logf(format string, v ...any) {
	logMu.RLock()
	fn := logfn
	logMu.RUnlock()
	fn(format, v...)
}

// Run calls fn on the calling goroutine. It recovers a panic from fn, logs
// the panic value with the stack, and reports whether fn panicked. The name
// identifies the work in the log line, for example "stats: lease loop".
//
// Use Run when the caller must react to the panic, for example to mark a
// lease lost. Use Go when a new goroutine is what you want.
func Run(name string, fn func()) (recovered bool) {
	defer func() {
		if p := recover(); p != nil {
			recovered = true
			logf("asynqmon: panic in %s: %v\n%s", name, p, debug.Stack())
		}
	}()
	fn()
	return false
}

// Go runs fn on a new goroutine and recovers a panic from fn. The panic
// value and the stack go to the logger. Deferred functions inside fn still
// run while the panic unwinds, so a fn that does "defer wg.Done()" keeps its
// WaitGroup contract.
func Go(name string, fn func()) {
	go Run(name, fn)
}

// GoLoop runs Loop on a new goroutine. It calls done, when done is not nil,
// after the loop returns; pass wg.Done to keep a WaitGroup contract that
// survives a restart, because Loop calls fn again instead of returning.
//
// Every background loop of asynqmon starts this way:
//
//	e.wg.Add(1)
//	safego.GoLoop(ctx, "stats: lease loop", e.wg.Done, func() { e.leaseLoop(ctx) })
func GoLoop(ctx context.Context, name string, done func(), fn func()) {
	Go(name, func() {
		if done != nil {
			defer done()
		}
		Loop(ctx, name, fn)
	})
}

// Loop calls fn on the calling goroutine and restarts fn after a panic,
// until ctx is done. Loop returns when fn returns without a panic, or when
// ctx is done. The pause before a restart starts at 100ms, doubles on each
// consecutive panic, and stops at 5s; a run that lasted a minute resets the
// pause to 100ms.
//
// Use Loop for a long-lived background loop, so that one bad tick does not
// stop the role for the lifetime of the process. Pair it with Go when the
// loop needs its own goroutine.
func Loop(ctx context.Context, name string, fn func()) {
	backoff := loopBackoffInitial
	for {
		if ctx.Err() != nil {
			return
		}
		start := time.Now()
		if !Run(name, fn) {
			return // fn finished its own work
		}
		if time.Since(start) >= loopHealthyRun {
			backoff = loopBackoffInitial
		}
		logf("asynqmon: restarting %s in %s", name, backoff)
		select {
		case <-ctx.Done():
			return
		case <-time.After(backoff):
		}
		backoff *= 2
		if backoff > loopBackoffMax {
			backoff = loopBackoffMax
		}
	}
}

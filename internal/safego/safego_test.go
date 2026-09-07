package safego

import (
	"context"
	"fmt"
	"strings"
	"sync"
	"testing"
	"time"

	. "github.com/smartystreets/goconvey/convey"
)

// ****************************************************************************
// Unit tests. No Redis, no network: every case drives Run, Go or Loop with a
// function that panics on purpose. The test binary itself proves the point —
// an unrecovered panic on any of these goroutines would fail the whole run.
// ****************************************************************************

// capture installs a logger that appends every line to a slice and restores
// the previous logger when the test ends.
type capture struct {
	mu    sync.Mutex
	lines []string
}

func newCapture(t *testing.T) *capture {
	t.Helper()
	c := &capture{}
	SetLogger(func(format string, v ...any) {
		c.mu.Lock()
		defer c.mu.Unlock()
		c.lines = append(c.lines, fmt.Sprintf(format, v...))
	})
	t.Cleanup(func() { SetLogger(nil) })
	return c
}

func (c *capture) all() string {
	c.mu.Lock()
	defer c.mu.Unlock()
	return strings.Join(c.lines, "\n")
}

func (c *capture) count() int {
	c.mu.Lock()
	defer c.mu.Unlock()
	return len(c.lines)
}

func TestRun(t *testing.T) {
	Convey("Given a function that returns normally", t, func() {
		c := newCapture(t)
		called := false

		Convey("When Run calls it", func() {
			recovered := Run("unit: quiet work", func() { called = true })

			Convey("Then it runs and Run reports no panic", func() {
				So(called, ShouldBeTrue)
				So(recovered, ShouldBeFalse)
				So(c.count(), ShouldEqual, 0)
			})
		})
	})

	Convey("Given a function that panics", t, func() {
		c := newCapture(t)

		Convey("When Run calls it", func() {
			recovered := Run("unit: exploding work", func() { panic("boom") })

			Convey("Then Run reports the panic and logs the name, the value and the stack", func() {
				So(recovered, ShouldBeTrue)
				out := c.all()
				So(out, ShouldContainSubstring, "asynqmon: panic in unit: exploding work: boom")
				So(out, ShouldContainSubstring, "safego.Run")
			})
		})
	})

	Convey("Given a function that panics after it defers cleanup", t, func() {
		newCapture(t)
		cleaned := false

		Convey("When Run calls it", func() {
			Run("unit: deferred cleanup", func() {
				defer func() { cleaned = true }()
				panic("boom")
			})

			Convey("Then the deferred cleanup still runs", func() {
				So(cleaned, ShouldBeTrue)
			})
		})
	})
}

func TestGo(t *testing.T) {
	Convey("Given a panicking function on its own goroutine", t, func() {
		c := newCapture(t)
		var wg sync.WaitGroup
		wg.Add(1)

		Convey("When Go runs it", func() {
			Go("unit: background work", func() {
				defer wg.Done()
				panic(fmt.Errorf("redis went away"))
			})
			wg.Wait()
			// The logger runs inside the same deferred recover as wg.Done,
			// so give that defer chain a moment to finish.
			waitFor(func() bool { return c.count() > 0 })

			Convey("Then the test binary survives and the log names the work", func() {
				out := c.all()
				So(out, ShouldContainSubstring, "asynqmon: panic in unit: background work: redis went away")
			})
		})
	})

	Convey("Given a WaitGroup-tracked fan-out where one worker panics", t, func() {
		newCapture(t)
		var wg sync.WaitGroup
		results := make([]int, 3)

		Convey("When Go runs every worker", func() {
			for i := range results {
				wg.Add(1)
				Go("unit: fan-out worker", func() {
					defer wg.Done()
					if i == 1 {
						panic("worker 1 is broken")
					}
					results[i] = i + 1
				})
			}
			wg.Wait()

			Convey("Then Wait returns and the healthy workers kept their results", func() {
				So(results, ShouldResemble, []int{1, 0, 3})
			})
		})
	})
}

func TestLoop(t *testing.T) {
	Convey("Given a function that panics on its first two runs", t, func() {
		c := newCapture(t)
		ctx, cancel := context.WithCancel(context.Background())
		defer cancel()
		var mu sync.Mutex
		runs := 0

		Convey("When Loop drives it", func() {
			done := make(chan struct{})
			Go("unit: loop host", func() {
				defer close(done)
				Loop(ctx, "unit: restarting loop", func() {
					mu.Lock()
					runs++
					n := runs
					mu.Unlock()
					if n <= 2 {
						panic("tick failed")
					}
				})
			})
			select {
			case <-done:
			case <-time.After(5 * time.Second):
				t.Fatal("Loop did not return within 5s")
			}

			Convey("Then it restarted after each panic and stopped on the clean run", func() {
				mu.Lock()
				defer mu.Unlock()
				So(runs, ShouldEqual, 3)
				So(c.all(), ShouldContainSubstring, "asynqmon: panic in unit: restarting loop: tick failed")
				So(c.all(), ShouldContainSubstring, "restarting unit: restarting loop in 100ms")
			})
		})
	})

	Convey("Given a function that always panics and a context that is canceled", t, func() {
		newCapture(t)
		ctx, cancel := context.WithCancel(context.Background())
		var mu sync.Mutex
		runs := 0

		Convey("When Loop drives it and the context is canceled", func() {
			done := make(chan struct{})
			Go("unit: loop host", func() {
				defer close(done)
				Loop(ctx, "unit: endless loop", func() {
					mu.Lock()
					runs++
					mu.Unlock()
					panic("always broken")
				})
			})
			waitFor(func() bool {
				mu.Lock()
				defer mu.Unlock()
				return runs >= 2
			})
			cancel()

			Convey("Then Loop returns instead of restarting forever", func() {
				select {
				case <-done:
				case <-time.After(5 * time.Second):
					t.Fatal("Loop did not stop after the context was canceled")
				}
				mu.Lock()
				defer mu.Unlock()
				So(runs, ShouldBeGreaterThanOrEqualTo, 2)
			})
		})
	})

	Convey("Given a context that is already done", t, func() {
		newCapture(t)
		ctx, cancel := context.WithCancel(context.Background())
		cancel()
		called := false

		Convey("When Loop is called", func() {
			Loop(ctx, "unit: never started", func() { called = true })

			Convey("Then it never calls the function", func() {
				So(called, ShouldBeFalse)
			})
		})
	})
}

func TestSetLogger(t *testing.T) {
	Convey("Given a captured logger", t, func() {
		c := newCapture(t)

		Convey("When another logger replaces it", func() {
			second := &capture{}
			SetLogger(func(format string, v ...any) {
				second.mu.Lock()
				defer second.mu.Unlock()
				second.lines = append(second.lines, fmt.Sprintf(format, v...))
			})
			Run("unit: after replacement", func() { panic("boom") })

			Convey("Then only the new logger receives the report", func() {
				So(c.count(), ShouldEqual, 0)
				So(second.all(), ShouldContainSubstring, "panic in unit: after replacement")
			})
		})
	})
}

// waitFor polls cond every millisecond for up to 2 seconds.
func waitFor(cond func() bool) {
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if cond() {
			return
		}
		time.Sleep(time.Millisecond)
	}
}

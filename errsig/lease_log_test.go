package errsig

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/hibiken/asynq"
	"github.com/redis/go-redis/v9"
	. "github.com/smartystreets/goconvey/convey"
)

// ****************************************************************************
// The indexer lease-failure log is rate limited.
//
// A Redis user without write permission never acquires the lease, so the
// acquire fails on every tick forever. This suite proves leaseTick reports
// that failure through the indexer's limiter, not through a bare logf. It
// drives the indexer clock, so it never sleeps. It uses the package's fence
// DB 6 (see fence_test.go).
// ****************************************************************************

// errNoPerm is the error a Redis ACL user without write permission gets for
// the lease script. The assertions match on "NOPERM", as an operator's logs
// would.
var errNoPerm = errors.New("NOPERM this user has no permissions to run the 'evalsha' command")

// noPermHook fails every command while its flag is set.
type noPermHook struct{ on *atomic.Bool }

func (h noPermHook) DialHook(next redis.DialHook) redis.DialHook { return next }

func (h noPermHook) ProcessHook(next redis.ProcessHook) redis.ProcessHook {
	return func(ctx context.Context, cmd redis.Cmder) error {
		if h.on.Load() {
			cmd.SetErr(errNoPerm)
			return errNoPerm
		}
		return next(ctx, cmd)
	}
}

func (h noPermHook) ProcessPipelineHook(next redis.ProcessPipelineHook) redis.ProcessPipelineHook {
	return func(ctx context.Context, cmds []redis.Cmder) error {
		if h.on.Load() {
			for _, c := range cmds {
				c.SetErr(errNoPerm)
			}
			return errNoPerm
		}
		return next(ctx, cmds)
	}
}

// newNoPermClient returns a client on the fence test DB whose commands fail
// while the returned flag is set.
func newNoPermClient(t *testing.T) (redis.UniversalClient, *atomic.Bool) {
	t.Helper()
	on := &atomic.Bool{}
	rc := redis.NewClient(&redis.Options{Addr: fenceTestRedisAddr, DB: fenceTestRedisDB})
	rc.AddHook(noPermHook{on: on})
	t.Cleanup(func() { rc.Close() })
	return rc, on
}

// leaseLinesMatching returns the captured lines that contain sub.
func leaseLinesMatching(lines []string, sub string) []string {
	var out []string
	for _, l := range lines {
		if strings.Contains(l, sub) {
			out = append(out, l)
		}
	}
	return out
}

func TestIndexerLeaseLogIsRateLimited(t *testing.T) {
	Convey("Given an indexer whose Redis refuses the lease script", t, func() {
		fenceTestRedis(t)
		insp := asynq.NewInspector(asynq.RedisClientOpt{Addr: fenceTestRedisAddr, DB: fenceTestRedisDB})
		t.Cleanup(func() { insp.Close() })
		rc, deny := newNoPermClient(t)
		ctx := context.Background()

		now := time.Date(2026, 9, 7, 12, 0, 0, 0, time.UTC)
		var lines []string
		ix := NewIndexer(Config{
			RedisClient: rc,
			Inspector:   insp,
			Now:         func() time.Time { return now },
			Logf: func(format string, args ...interface{}) {
				lines = append(lines, fmt.Sprintf(format, args...))
			},
		})
		deny.Store(true)

		Convey("When the lease tick runs 13 times over a minute", func() {
			for i := 0; i < 13; i++ {
				ix.leaseTick(ctx)
				now = now.Add(5 * time.Second)
			}
			acquire := leaseLinesMatching(lines, "acquiring indexer lease")

			Convey("Then the first line keeps its original wording", func() {
				So(len(acquire), ShouldBeGreaterThan, 0)
				So(acquire[0], ShouldEqual, "asynqmon: errsig: acquiring indexer lease: "+errNoPerm.Error())
			})

			Convey("Then two lines are logged instead of 13", func() {
				So(len(acquire), ShouldEqual, 2)
				So(acquire[1], ShouldContainSubstring, "13 times")
			})

			Convey("And when the lease script works again", func() {
				deny.Store(false)
				ix.leaseTick(ctx)

				Convey("Then exactly one recovery line is logged", func() {
					So(len(leaseLinesMatching(lines, "recovered after 13 failures")), ShouldEqual, 1)
					So(ix.LeaseHeld(), ShouldBeTrue)
				})
			})
		})
	})
}

func TestIndexerLeaseRenewLogUsesItsOwnKey(t *testing.T) {
	Convey("Given an indexer that holds the indexer lease", t, func() {
		fenceTestRedis(t)
		insp := asynq.NewInspector(asynq.RedisClientOpt{Addr: fenceTestRedisAddr, DB: fenceTestRedisDB})
		t.Cleanup(func() { insp.Close() })
		rc, deny := newNoPermClient(t)
		ctx := context.Background()

		now := time.Date(2026, 9, 7, 12, 0, 0, 0, time.UTC)
		var lines []string
		ix := NewIndexer(Config{
			RedisClient: rc,
			Inspector:   insp,
			Now:         func() time.Time { return now },
			Logf: func(format string, args ...interface{}) {
				lines = append(lines, fmt.Sprintf(format, args...))
			},
		})
		ix.leaseTick(ctx)
		So(ix.LeaseHeld(), ShouldBeTrue)

		Convey("When every command fails and two ticks run", func() {
			deny.Store(true)
			ix.leaseTick(ctx) // renewal fails, the indexer drops to standby
			ix.leaseTick(ctx) // the acquire then fails too

			Convey("Then the renewal line keeps its original wording", func() {
				renew := leaseLinesMatching(lines, "renewing indexer lease")
				So(len(renew), ShouldEqual, 1)
				So(renew[0], ShouldEqual, "asynqmon: errsig: renewing indexer lease: "+errNoPerm.Error())
			})

			Convey("Then the acquire failure is not masked by the renewal failure", func() {
				acquire := leaseLinesMatching(lines, "acquiring indexer lease")
				So(len(acquire), ShouldEqual, 1)
				So(acquire[0], ShouldEqual, "asynqmon: errsig: acquiring indexer lease: "+errNoPerm.Error())
			})
		})
	})
}

package leasefence

import (
	"context"
	"testing"
	"time"

	"github.com/redis/go-redis/v9"
	. "github.com/smartystreets/goconvey/convey"
)

// ****************************************************************************
// Integration tests against a real Redis on 127.0.0.1:6379, DB 15 (unused by
// any other suite: root handlers use 7-13, stats 14). Skipped when Redis does
// not answer PING.
// ****************************************************************************

const (
	testRedisAddr = "127.0.0.1:6379"
	testRedisDB   = 15
)

func testRedis(t *testing.T) redis.UniversalClient {
	t.Helper()
	rc := redis.NewClient(&redis.Options{Addr: testRedisAddr, DB: testRedisDB})
	ctx := context.Background()
	if err := rc.Ping(ctx).Err(); err != nil {
		rc.Close()
		t.Skipf("skipping: redis not available on %s: %v", testRedisAddr, err)
	}
	if err := rc.FlushDB(ctx).Err(); err != nil {
		rc.Close()
		t.Fatalf("flushing test db: %v", err)
	}
	t.Cleanup(func() { rc.Close() })
	return rc
}

func TestAcquireRenewRelease(t *testing.T) {
	rc := testRedis(t)
	ctx := context.Background()
	f := New(rc, "testrole")

	Convey("Given a free role lease", t, func() {
		// Ancestor blocks re-execute once per leaf (goconvey): reset state so
		// every leaf sees a genuinely free lease.
		So(rc.FlushDB(ctx).Err(), ShouldBeNil)
		Convey("When the first instance acquires", func() {
			tok1, err := f.Acquire(ctx, "inst-1", time.Minute)
			So(err, ShouldBeNil)
			So(tok1, ShouldBeGreaterThan, 0)

			Convey("Then a second instance cannot acquire while it is held", func() {
				tok2, err := f.Acquire(ctx, "inst-2", time.Minute)
				So(err, ShouldBeNil)
				So(tok2, ShouldEqual, 0)
			})

			Convey("Then only the holder can renew", func() {
				ok, err := f.Renew(ctx, "inst-1", time.Minute)
				So(err, ShouldBeNil)
				So(ok, ShouldBeTrue)
				ok, err = f.Renew(ctx, "inst-2", time.Minute)
				So(err, ShouldBeNil)
				So(ok, ShouldBeFalse)
			})

			Convey("Then release by a non-holder is a no-op and by the holder frees it", func() {
				So(f.Release(ctx, "inst-2"), ShouldBeNil)
				So(rc.Exists(ctx, LockKey("testrole")).Val(), ShouldEqual, 1)
				So(f.Release(ctx, "inst-1"), ShouldBeNil)
				So(rc.Exists(ctx, LockKey("testrole")).Val(), ShouldEqual, 0)

				Convey("And the next acquisition mints a strictly higher token", func() {
					tok2, err := f.Acquire(ctx, "inst-2", time.Minute)
					So(err, ShouldBeNil)
					So(tok2, ShouldBeGreaterThan, tok1)
				})
			})
		})
	})
}

func TestFencedExec(t *testing.T) {
	rc := testRedis(t)
	ctx := context.Background()
	f := New(rc, "execrole")

	Convey("Given a held lease with a current token", t, func() {
		// Ancestor blocks re-execute once per leaf (goconvey), so reset the
		// lease state here to make each acquisition fresh.
		So(rc.FlushDB(ctx).Err(), ShouldBeNil)
		tok1, err := f.Acquire(ctx, "inst-1", time.Minute)
		So(err, ShouldBeNil)
		So(tok1, ShouldBeGreaterThan, 0)

		Convey("When the current holder writes a fenced batch", func() {
			ok, err := f.Exec(ctx, tok1, []Cmd{
				{"SET", "lf:test:a", "1"},
				{"HSET", "lf:test:h", "f1", "v1", "f2", "v2"},
			})
			So(err, ShouldBeNil)
			So(ok, ShouldBeTrue)

			Convey("Then every command landed", func() {
				So(rc.Get(ctx, "lf:test:a").Val(), ShouldEqual, "1")
				So(rc.HGet(ctx, "lf:test:h", "f2").Val(), ShouldEqual, "v2")
			})
		})

		Convey("When a newer claimant supersedes the token", func() {
			So(f.Release(ctx, "inst-1"), ShouldBeNil)
			tok2, err := f.Acquire(ctx, "inst-2", time.Minute)
			So(err, ShouldBeNil)
			So(tok2, ShouldBeGreaterThan, tok1)

			Convey("Then the stale holder's write is rejected and nothing lands", func() {
				ok, err := f.Exec(ctx, tok1, []Cmd{{"SET", "lf:test:stale", "boom"}})
				So(err, ShouldBeNil)
				So(ok, ShouldBeFalse)
				So(rc.Exists(ctx, "lf:test:stale").Val(), ShouldEqual, 0)
			})

			Convey("And the new holder's write succeeds", func() {
				ok, err := f.Exec(ctx, tok2, []Cmd{{"SET", "lf:test:fresh", "yes"}})
				So(err, ShouldBeNil)
				So(ok, ShouldBeTrue)
				So(rc.Get(ctx, "lf:test:fresh").Val(), ShouldEqual, "yes")
			})
		})

		Convey("When a batch larger than one chunk is written", func() {
			cmds := make([]Cmd, 0, execChunk+50)
			for i := 0; i < execChunk+50; i++ {
				cmds = append(cmds, Cmd{"RPUSH", "lf:test:list", "x"})
			}
			ok, err := f.Exec(ctx, tok1, cmds)
			So(err, ShouldBeNil)
			So(ok, ShouldBeTrue)
			So(rc.LLen(ctx, "lf:test:list").Val(), ShouldEqual, int64(execChunk+50))
		})

		Convey("When token 0 is passed (explicit unfenced operational write)", func() {
			ok, err := f.Exec(ctx, 0, []Cmd{{"SET", "lf:test:unfenced", "ok"}})
			So(err, ShouldBeNil)
			So(ok, ShouldBeTrue)
			So(rc.Get(ctx, "lf:test:unfenced").Val(), ShouldEqual, "ok")
		})
	})
}

func TestHolder(t *testing.T) {
	rc := testRedis(t)
	ctx := context.Background()
	f := New(rc, "holderrole")

	Convey("Given a role", t, func() {
		Convey("When no one holds the lease", func() {
			info, err := f.Holder(ctx)
			So(err, ShouldBeNil)
			So(info.Holder, ShouldEqual, "")
			So(info.TTL, ShouldEqual, 0)
		})

		Convey("When an instance holds the lease", func() {
			tok, err := f.Acquire(ctx, "inst-9", time.Minute)
			So(err, ShouldBeNil)
			So(tok, ShouldBeGreaterThan, 0)
			info, err := f.Holder(ctx)
			So(err, ShouldBeNil)
			So(info.Role, ShouldEqual, "holderrole")
			So(info.Holder, ShouldEqual, "inst-9")
			So(info.TTL, ShouldBeGreaterThan, 50*time.Second)
			So(info.Fence, ShouldEqual, tok)
		})
	})
}

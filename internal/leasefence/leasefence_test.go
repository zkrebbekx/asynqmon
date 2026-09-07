package leasefence

import (
	"context"
	"strconv"
	"strings"
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
	testRedisDB = 15
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

var testRedisAddr = testRedisAddrFromEnv()

// TestTokenFormat: the fence key holds "<seconds>:<sequence>", the sequence
// grows by one per claim, and the seconds come from the Redis clock.
func TestTokenFormat(t *testing.T) {
	rc := testRedis(t)
	ctx := context.Background()
	f := New(rc, "tokrole")

	Convey("Given a role whose lease is claimed twice", t, func() {
		So(rc.FlushDB(ctx).Err(), ShouldBeNil)
		tok1, err := f.Acquire(ctx, "inst-1", time.Minute)
		So(err, ShouldBeNil)
		So(f.Release(ctx, "inst-1"), ShouldBeNil)
		tok2, err := f.Acquire(ctx, "inst-2", time.Minute)
		So(err, ShouldBeNil)

		Convey("Then the stored token is <redis TIME seconds>:<sequence>", func() {
			raw := rc.Get(ctx, FenceKey("tokrole")).Val()
			parts := strings.Split(raw, ":")
			So(len(parts), ShouldEqual, 2)
			secs, err := strconv.ParseInt(parts[0], 10, 64)
			So(err, ShouldBeNil)
			So(secs, ShouldBeGreaterThan, time.Now().Add(-time.Hour).Unix())
			So(parts[1], ShouldEqual, "2")
		})
		Convey("Then the int64 token encodes the same pair and grows per claim", func() {
			So(TokenSeq(tok1), ShouldEqual, 1)
			So(TokenSeq(tok2), ShouldEqual, 2)
			So(tok2, ShouldBeGreaterThan, tok1)
			So(TokenTime(tok2).Unix(), ShouldBeGreaterThan, time.Now().Add(-time.Hour).Unix())
			So(FormatToken(tok2), ShouldEqual, rc.Get(ctx, FenceKey("tokrole")).Val())
		})
		Convey("Then a legacy bare-integer fence value still parses and orders", func() {
			legacy, err := ParseToken("7")
			So(err, ShouldBeNil)
			So(legacy, ShouldEqual, int64(7))
			So(legacy, ShouldBeLessThan, tok1)
			_, err = ParseToken("not-a-token")
			So(err, ShouldNotBeNil)
		})
	})
}

// TestExecRequiresPossession: the current token alone is not enough — the
// lock key must still name this instance (#52.1).
func TestExecRequiresPossession(t *testing.T) {
	rc := testRedis(t)
	ctx := context.Background()
	f := New(rc, "possrole")

	Convey("Given an instance whose lease expired but whose token is still current", t, func() {
		So(rc.FlushDB(ctx).Err(), ShouldBeNil)
		tok, err := f.Acquire(ctx, "inst-1", time.Minute)
		So(err, ShouldBeNil)
		So(tok, ShouldBeGreaterThan, 0)
		// The token stays the newest one; only the possession is gone.
		So(rc.Del(ctx, LockKey("possrole")).Err(), ShouldBeNil)
		So(rc.Get(ctx, FenceKey("possrole")).Val(), ShouldEqual, FormatToken(tok))

		Convey("When it writes a fenced batch", func() {
			ok, err := f.Exec(ctx, tok, []Cmd{{"SET", "lf:poss:a", "1"}})

			Convey("Then the write is rejected and nothing lands", func() {
				So(err, ShouldBeNil)
				So(ok, ShouldBeFalse)
				So(rc.Exists(ctx, "lf:poss:a").Val(), ShouldEqual, 0)
			})
		})

		Convey("When another instance takes the lease with the same token value", func() {
			So(rc.Set(ctx, LockKey("possrole"), "inst-2", time.Minute).Err(), ShouldBeNil)

			Convey("Then the first instance still cannot write", func() {
				ok, err := f.Exec(ctx, tok, []Cmd{{"SET", "lf:poss:b", "1"}})
				So(err, ShouldBeNil)
				So(ok, ShouldBeFalse)
				So(rc.Exists(ctx, "lf:poss:b").Val(), ShouldEqual, 0)
			})
		})
	})
}

// TestWideCommandChunking: a command with 9000 members goes through Exec as
// several commands and every member lands (#52.2).
func TestWideCommandChunking(t *testing.T) {
	rc := testRedis(t)
	ctx := context.Background()
	f := New(rc, "widerole")

	Convey("Given a fenced batch with a 9000-member SADD", t, func() {
		So(rc.FlushDB(ctx).Err(), ShouldBeNil)
		tok, err := f.Acquire(ctx, "inst-1", time.Minute)
		So(err, ShouldBeNil)

		sadd := Cmd{"SADD", "lf:wide:set"}
		for i := 0; i < 9000; i++ {
			sadd = append(sadd, "member-"+strconv.Itoa(i))
		}
		hset := Cmd{"HSET", "lf:wide:hash"}
		for i := 0; i < 3000; i++ {
			hset = append(hset, "f"+strconv.Itoa(i), "v"+strconv.Itoa(i))
		}

		Convey("When the holder executes it", func() {
			ok, err := f.Exec(ctx, tok, []Cmd{sadd, hset})

			Convey("Then the write succeeds and every member landed", func() {
				So(err, ShouldBeNil)
				So(ok, ShouldBeTrue)
				So(rc.SCard(ctx, "lf:wide:set").Val(), ShouldEqual, int64(9000))
				So(rc.SIsMember(ctx, "lf:wide:set", "member-8999").Val(), ShouldBeTrue)
				So(rc.HLen(ctx, "lf:wide:hash").Val(), ShouldEqual, int64(3000))
			})
		})
	})
}

// TestSplitWide covers the splitting rules without Redis.
func TestSplitWide(t *testing.T) {
	Convey("Given commands of several shapes", t, func() {
		Convey("Then a narrow command passes through unchanged", func() {
			in := []Cmd{{"SET", "k", "v"}, {"SADD", "s", "a", "b"}}
			So(len(splitWide(in)), ShouldEqual, 2)
		})
		Convey("Then a wide SADD splits into 1000-member pieces", func() {
			c := Cmd{"SADD", "s"}
			for i := 0; i < 2500; i++ {
				c = append(c, i)
			}
			out := splitWide([]Cmd{c})
			So(len(out), ShouldEqual, 3)
			So(len(out[0]), ShouldEqual, 1002)
			So(len(out[2]), ShouldEqual, 502)
			So(out[2][0], ShouldEqual, "SADD")
			So(out[2][1], ShouldEqual, "s")
		})
		Convey("Then a wide ZADD keeps its options and splits on score/member pairs", func() {
			c := Cmd{"ZADD", "z", "GT", "CH"}
			for i := 0; i < 1500; i++ {
				c = append(c, float64(i), "m"+strconv.Itoa(i))
			}
			out := splitWide([]Cmd{c})
			So(len(out), ShouldEqual, 2)
			So(out[1][2], ShouldEqual, "GT")
			So(out[1][3], ShouldEqual, "CH")
			So(len(out[1]), ShouldEqual, 4+2*500)
		})
		Convey("Then a wide DEL splits over its keys (no key prefix)", func() {
			c := Cmd{"DEL"}
			for i := 0; i < 1200; i++ {
				c = append(c, "k"+strconv.Itoa(i))
			}
			out := splitWide([]Cmd{c})
			So(len(out), ShouldEqual, 2)
			So(len(out[0]), ShouldEqual, 1001)
			So(len(out[1]), ShouldEqual, 201)
		})
		Convey("Then an odd-length HSET is left alone rather than split wrongly", func() {
			c := Cmd{"HSET", "h"}
			for i := 0; i < 2001; i++ {
				c = append(c, "x")
			}
			So(len(splitWide([]Cmd{c})), ShouldEqual, 1)
		})
	})
}

// TestStandDown covers the stale-rejection guard (#52.1).
func TestStandDown(t *testing.T) {
	Convey("Given an engine holding a token", t, func() {
		var token int64 = 42
		var holding int32 = 1

		Convey("When a write with that token is rejected", func() {
			cleared := StandDown(&token, &holding, 42)
			Convey("Then the engine stands down", func() {
				So(cleared, ShouldBeTrue)
				So(token, ShouldEqual, int64(0))
				So(holding, ShouldEqual, int32(0))
			})
		})
		Convey("When a write with an OLDER token is rejected", func() {
			cleared := StandDown(&token, &holding, 41)
			Convey("Then the stand-down is skipped and the newer lease is kept", func() {
				So(cleared, ShouldBeFalse)
				So(token, ShouldEqual, int64(42))
				So(holding, ShouldEqual, int32(1))
			})
		})
		Convey("When the rejected token is 0 (an unfenced write)", func() {
			cleared := StandDown(&token, &holding, 0)
			Convey("Then nothing changes", func() {
				So(cleared, ShouldBeFalse)
				So(token, ShouldEqual, int64(42))
			})
		})
	})
}

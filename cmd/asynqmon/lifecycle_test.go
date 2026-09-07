package main

import (
	"context"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/hibiken/asynq"
	"github.com/redis/go-redis/v9"
	. "github.com/smartystreets/goconvey/convey"
)

// ****************************************************************************
// Lifecycle tests for the asynqmon binary on DB 14 (this suite's database):
//   - run(ctx, cfg, onListen) releases every singleton lease on SIGTERM (#28)
//   - the Redis socket budget bounds /healthz against a hung server (#39)
//   - --purge-owned-keys deletes asynqmon:* and spares asynq:* (#45)
// Every Redis-backed test skips when Redis does not answer PING.
// ****************************************************************************

const lifecycleTestRedisDB = 14

func lifecycleTestRedisAddr() string {
	if v := os.Getenv("ASYNQMON_TEST_REDIS_ADDR"); v != "" {
		return v
	}
	return "127.0.0.1:6379"
}

// newLifecycleRedis returns a flushed client on the suite's database, or
// skips the test when Redis does not answer.
func newLifecycleRedis(t *testing.T) *redis.Client {
	t.Helper()
	rc := redis.NewClient(&redis.Options{Addr: lifecycleTestRedisAddr(), DB: lifecycleTestRedisDB})
	ctx := context.Background()
	if err := rc.Ping(ctx).Err(); err != nil {
		rc.Close()
		t.Skipf("skipping: redis not available on %s: %v", lifecycleTestRedisAddr(), err)
	}
	if err := rc.FlushDB(ctx).Err(); err != nil {
		rc.Close()
		t.Fatalf("flushing db %d: %v", lifecycleTestRedisDB, err)
	}
	t.Cleanup(func() { rc.Close() })
	return rc
}

// freePort returns a TCP port with no listener on it.
func freePort(t *testing.T) int {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("reserving a port: %v", err)
	}
	defer ln.Close()
	return ln.Addr().(*net.TCPAddr).Port
}

// waitFor polls cond every 10ms until it holds or the budget expires.
// It reports whether cond held.
func waitFor(budget time.Duration, cond func() bool) bool {
	deadline := time.Now().Add(budget)
	for time.Now().Before(deadline) {
		if cond() {
			return true
		}
		time.Sleep(10 * time.Millisecond)
	}
	return cond()
}

// TestRunReleasesLeasesOnShutdown is the #28 regression test. The old main
// called log.Fatal(srv.ListenAndServe()) and handled no signal, so h.Close
// never ran and asynqmon:lock:stats stayed held by the dead process for the
// rest of its 15s TTL. Canceling run's context must now release it: PTTL
// reports -2 (no key) within 1s of run returning.
func TestRunReleasesLeasesOnShutdown(t *testing.T) {
	rc := newLifecycleRedis(t)
	ctx, cancel := context.WithCancel(context.Background())
	cfg := &Config{
		Port:          freePort(t),
		RedisAddr:     lifecycleTestRedisAddr(),
		RedisDB:       lifecycleTestRedisDB,
		StatsInterval: 50 * time.Millisecond,
		// The bulk-job runner and the hygiene scheduler take leases of
		// their own; leaving them on proves every engine releases.
		JobConcurrency: 1,
	}

	listening := make(chan string, 1)
	runErr := make(chan error, 1)
	go func() {
		runErr <- run(ctx, cfg, func(addr net.Addr) { listening <- addr.String() })
	}()

	var addr string
	select {
	case addr = <-listening:
	case err := <-runErr:
		t.Fatalf("run returned before listening: %v", err)
	case <-time.After(10 * time.Second):
		t.Fatal("run never reported a listener")
	}

	// Wait until the stats sweeper actually holds its lease, otherwise the
	// test would pass on a lease that was never taken.
	lockKey := "asynqmon:lock:stats"
	acquired := waitFor(10*time.Second, func() bool {
		return rc.Exists(context.Background(), lockKey).Val() == 1
	})

	// The listener serves while run is up.
	statusBeforeShutdown := 0
	if resp, err := http.Get("http://" + addr + "/healthz"); err == nil {
		statusBeforeShutdown = resp.StatusCode
		resp.Body.Close()
	}

	cancel()
	var shutdownErr error
	select {
	case shutdownErr = <-runErr:
	case <-time.After(20 * time.Second):
		t.Fatal("run did not return after its context was canceled")
	}

	// PTTL -2 means the key does not exist: the lease was released, not
	// left to expire. go-redis reports the -2 sentinel verbatim, so the
	// value is time.Duration(-2), not -2ms.
	const pttlKeyAbsent = time.Duration(-2)
	released := waitFor(time.Second, func() bool {
		return rc.PTTL(context.Background(), lockKey).Val() == pttlKeyAbsent
	})
	pttl := rc.PTTL(context.Background(), lockKey).Val()

	Convey("Given the asynqmon server running against redis (#28)", t, func() {
		Convey("When the server is up", func() {
			Convey("Then it serves /healthz and holds the stats lease", func() {
				So(statusBeforeShutdown, ShouldEqual, http.StatusOK)
				So(acquired, ShouldBeTrue)
			})
		})
		Convey("When the process receives a shutdown signal", func() {
			Convey("Then run returns without an error", func() {
				So(shutdownErr, ShouldBeNil)
			})
			Convey("Then every singleton lease is released within 1s", func() {
				So(released, ShouldBeTrue)
				So(pttl, ShouldEqual, pttlKeyAbsent) // -2 = key absent
				So(rc.Exists(context.Background(), "asynqmon:lock:errsig").Val(), ShouldEqual, 0)
				So(rc.Exists(context.Background(), "asynqmon:lock:hygiene").Val(), ShouldEqual, 0)
			})
			Convey("Then the listener is closed", func() {
				_, err := http.Get("http://" + addr + "/healthz")
				So(err, ShouldNotBeNil)
			})
		})
	})
}

// hungRedis accepts TCP connections and never replies. It reproduces the #39
// repro: go-redis applied its own 3s ReadTimeout and ignored the caller's
// context, so /healthz answered at about 3.0s despite its 2s budget.
type hungRedis struct {
	ln    net.Listener
	conns chan net.Conn
}

func newHungRedis(t *testing.T) *hungRedis {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("starting the hung redis stub: %v", err)
	}
	h := &hungRedis{ln: ln, conns: make(chan net.Conn, 64)}
	go func() {
		for {
			c, err := ln.Accept()
			if err != nil {
				return
			}
			h.conns <- c // held open, never read from, never written to
		}
	}()
	t.Cleanup(func() {
		ln.Close()
		close(h.conns)
		for c := range h.conns {
			c.Close()
		}
	})
	return h
}

func (h *hungRedis) addr() string { return h.ln.Addr().String() }

// TestHealthzAnswersWithinBudgetAgainstHungRedis is the #39 regression test.
func TestHealthzAnswersWithinBudgetAgainstHungRedis(t *testing.T) {
	hung := newHungRedis(t)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	cfg := &Config{
		Port:      freePort(t),
		RedisAddr: hung.addr(),
		// The library-side budget for /healthz is 2s; the socket timeouts
		// must be at or below it.
		RedisTimeout: 2 * time.Second,
		DisableStats: true,
	}

	listening := make(chan string, 1)
	runErr := make(chan error, 1)
	go func() { runErr <- run(ctx, cfg, func(addr net.Addr) { listening <- addr.String() }) }()

	var addr string
	select {
	case addr = <-listening:
	case err := <-runErr:
		t.Fatalf("run returned before listening: %v", err)
	case <-time.After(10 * time.Second):
		t.Fatal("run never reported a listener")
	}

	client := &http.Client{Timeout: 10 * time.Second}
	start := time.Now()
	resp, err := client.Get("http://" + addr + "/healthz")
	elapsed := time.Since(start)
	status, body := 0, ""
	if err == nil {
		status = resp.StatusCode
		b, _ := io.ReadAll(resp.Body)
		body = string(b)
		resp.Body.Close()
	}

	cancel()
	<-runErr

	Convey("Given a redis that accepts connections and never replies (#39)", t, func() {
		Convey("When /healthz is probed", func() {
			Convey("Then it answers 503 inside its 2s budget", func() {
				So(err, ShouldBeNil)
				So(status, ShouldEqual, http.StatusServiceUnavailable)
				So(elapsed, ShouldBeLessThan, 2500*time.Millisecond)
			})
			Convey("Then the body names no redis address", func() {
				So(body, ShouldContainSubstring, "unavailable")
				So(body, ShouldNotContainSubstring, hung.addr())
			})
		})
		Convey("When the listener came up", func() {
			Convey("Then it bound its port without waiting for redis", func() {
				So(addr, ShouldNotBeEmpty)
			})
		})
	})
}

// TestPurgeOwnedKeys is the #45 item-2 test: --purge-owned-keys deletes every
// asynqmon:* key and leaves the asynq:* queue keys untouched.
func TestPurgeOwnedKeys(t *testing.T) {
	rc := newLifecycleRedis(t)
	ctx := context.Background()

	owned := []string{
		"asynqmon:lock:stats",
		"asynqmon:views:index",
		"asynqmon:markers",
		"asynqmon:series:since",
	}
	foreign := []string{
		"asynq:queues",
		"asynq:{default}:pending",
	}
	for _, k := range append(append([]string{}, owned...), foreign...) {
		if err := rc.Set(ctx, k, "x", 0).Err(); err != nil {
			t.Fatalf("seeding %s: %v", k, err)
		}
	}

	res, purgeErr := purgeOwnedKeys(ctx, rc)

	ownedLeft := rc.Exists(ctx, owned...).Val()
	foreignLeft := rc.Exists(ctx, foreign...).Val()

	// A second purge on a clean database is a no-op, not an error.
	empty, emptyErr := purgeOwnedKeys(ctx, rc)

	Convey("Given a redis holding asynqmon-owned and asynq keys (#45)", t, func() {
		Convey("When --purge-owned-keys runs", func() {
			Convey("Then it deletes every asynqmon:* key", func() {
				So(purgeErr, ShouldBeNil)
				So(res.Matched, ShouldEqual, len(owned))
				So(res.Deleted, ShouldEqual, len(owned))
				So(ownedLeft, ShouldEqual, 0)
			})
			Convey("Then it leaves the asynq:* queue keys untouched", func() {
				So(foreignLeft, ShouldEqual, int64(len(foreign)))
			})
		})
		Convey("When it runs again with nothing left to purge", func() {
			Convey("Then it reports zero counts and no error", func() {
				So(emptyErr, ShouldBeNil)
				So(empty.Matched, ShouldEqual, 0)
				So(empty.Deleted, ShouldEqual, 0)
			})
		})
	})
}

// TestRunPurgeReportsCounts covers the CLI wrapper: it connects, purges, and
// prints the counts.
func TestRunPurgeReportsCounts(t *testing.T) {
	rc := newLifecycleRedis(t)
	ctx := context.Background()
	if err := rc.Set(ctx, "asynqmon:markers", "x", 0).Err(); err != nil {
		t.Fatalf("seeding: %v", err)
	}

	f, err := os.CreateTemp(t.TempDir(), "purge-out")
	if err != nil {
		t.Fatalf("creating the output file: %v", err)
	}
	defer f.Close()
	purgeErr := runPurge(ctx, &Config{
		RedisAddr: lifecycleTestRedisAddr(),
		RedisDB:   lifecycleTestRedisDB,
	}, f)
	out, _ := os.ReadFile(f.Name())

	Convey("Given the --purge-owned-keys command (#45)", t, func() {
		Convey("When it runs against a database holding one owned key", func() {
			Convey("Then it succeeds and prints the counts", func() {
				So(purgeErr, ShouldBeNil)
				So(string(out), ShouldContainSubstring, "purged 1")
				So(rc.Exists(ctx, "asynqmon:markers").Val(), ShouldEqual, 0)
			})
		})
	})
}

// TestLogRedisVersion covers the #37 startup probe helpers: the INFO reply
// parser and the minimum-version comparison.
func TestLogRedisVersion(t *testing.T) {
	rc := newLifecycleRedis(t)
	live := parseRedisVersion(rc.Info(context.Background(), "server").Val())

	Convey("Given the startup redis-version probe (#37)", t, func() {
		Convey("When an INFO server reply is parsed", func() {
			Convey("Then redis_version is extracted", func() {
				So(parseRedisVersion("# Server\r\nredis_version:7.2.4\r\nredis_mode:standalone\r\n"), ShouldEqual, "7.2.4")
			})
			Convey("Then a reply without the field yields an empty string", func() {
				So(parseRedisVersion("# Server\r\nredis_mode:standalone\r\n"), ShouldBeEmpty)
			})
			Convey("Then the live server reports a version", func() {
				So(live, ShouldNotBeEmpty)
			})
		})
		Convey("When a version is compared with the 6.2 minimum", func() {
			Convey("Then versions below 6.2 are reported as below", func() {
				So(redisVersionBelow("6.0.16", 6, 2), ShouldBeTrue)
				So(redisVersionBelow("5.0.14", 6, 2), ShouldBeTrue)
			})
			Convey("Then 6.2 and later are not", func() {
				So(redisVersionBelow("6.2.0", 6, 2), ShouldBeFalse)
				So(redisVersionBelow("7.2.4", 6, 2), ShouldBeFalse)
				So(redisVersionBelow("10.0.1", 6, 2), ShouldBeFalse)
			})
			Convey("Then an unparsable version raises no false warning", func() {
				So(redisVersionBelow("unstable", 6, 2), ShouldBeFalse)
				So(redisVersionBelow("6", 6, 2), ShouldBeFalse)
			})
		})
		Convey("When the probe runs against an unreachable redis", func() {
			Convey("Then it returns instead of blocking the boot", func() {
				done := make(chan struct{})
				go func() {
					defer close(done)
					logRedisVersion(context.Background(), asynq.RedisClientOpt{
						Addr:        "127.0.0.1:1",
						DialTimeout: 200 * time.Millisecond,
					})
				}()
				select {
				case <-done:
				case <-time.After(5 * time.Second):
					So(fmt.Errorf("logRedisVersion blocked"), ShouldBeNil)
				}
			})
		})
	})
}

// TestVersionStamp covers the #46 version variable and its --version flag.
func TestVersionStamp(t *testing.T) {
	cfg, output, err := parseFlags("asynqmon", []string{"--version"})

	Convey("Given the build version stamp (#46)", t, func() {
		Convey("When the binary is built without -ldflags", func() {
			Convey("Then the version reads devel", func() {
				So(version, ShouldEqual, "devel")
			})
		})
		Convey("When --version is passed", func() {
			Convey("Then the flag parses and requests the version", func() {
				So(err, ShouldBeNil)
				So(output, ShouldBeEmpty)
				So(cfg.ShowVersion, ShouldBeTrue)
			})
		})
		Convey("When the options are built", func() {
			opts := buildOptions(&Config{}, asynq.RedisClientOpt{Addr: "127.0.0.1:6379"})
			Convey("Then the version reaches Options.Version for /api/features", func() {
				So(opts.Version, ShouldEqual, version)
			})
		})
	})
}

// TestValidateConfig covers the startup config checks, including the
// --trusted-proxies parse that used to panic inside the library at boot.
func TestValidateConfig(t *testing.T) {
	Convey("Given the startup config validation", t, func() {
		Convey("When --require-identity and --auth-header carry no --trusted-proxies", func() {
			err := validateConfig(&Config{RequireIdentity: true, AuthHeader: "X-Auth-Request-User"})
			Convey("Then the binary refuses the spoofable configuration", func() {
				So(err, ShouldNotBeNil)
				So(err.Error(), ShouldContainSubstring, "--trusted-proxies")
			})
		})
		Convey("When --trusted-proxies carries a malformed entry", func() {
			err := validateConfig(&Config{TrustedProxies: "10.0.0.0/8,not-an-ip"})
			Convey("Then it is rejected before the server starts", func() {
				So(err, ShouldNotBeNil)
				So(err.Error(), ShouldContainSubstring, "not-an-ip")
			})
		})
		Convey("When the configuration is valid", func() {
			err := validateConfig(&Config{TrustedProxies: "10.0.0.0/8, 192.168.1.1 "})
			Convey("Then it passes", func() {
				So(err, ShouldBeNil)
			})
		})
	})
}

// TestClusterGate covers #37 option (a): --redis-cluster-nodes forces every
// write feature off, because the fenced Lua batches behind them touch keys
// outside the declared KEYS and Redis Cluster rejects that.
func TestClusterGate(t *testing.T) {
	connOpt := asynq.RedisClientOpt{Addr: "127.0.0.1:6379"}
	single := buildOptions(&Config{RedisAddr: "127.0.0.1:6379"}, connOpt)
	cluster := buildOptions(&Config{RedisClusterNodes: "10.0.0.1:6379,10.0.0.2:6379"}, connOpt)

	Convey("Given the redis-cluster startup gate (#37)", t, func() {
		Convey("When a single redis server is configured", func() {
			Convey("Then every background feature stays enabled", func() {
				So(single.StatsDisabled, ShouldBeFalse)
				So(single.ErrorIndexDisabled, ShouldBeFalse)
				So(single.HygieneDisabled, ShouldBeFalse)
				So(single.JobsDisabled, ShouldBeFalse)
			})
		})
		Convey("When --redis-cluster-nodes is set", func() {
			Convey("Then the four write features are forced off", func() {
				So(cluster.StatsDisabled, ShouldBeTrue)
				So(cluster.ErrorIndexDisabled, ShouldBeTrue)
				So(cluster.HygieneDisabled, ShouldBeTrue)
				So(cluster.JobsDisabled, ShouldBeTrue)
			})
			Convey("Then the notice names the unavailable features", func() {
				So(clusterGateNotice, ShouldContainSubstring, "Redis Cluster")
				So(strings.Contains(clusterGateNotice, "write features"), ShouldBeTrue)
			})
		})
	})
}

// TestSecurityHeadersEndToEnd serves the real binary and checks that both the
// SPA root and a live API route carry the security headers (#34).
func TestSecurityHeadersEndToEnd(t *testing.T) {
	newLifecycleRedis(t)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	cfg := &Config{
		Port:         freePort(t),
		RedisAddr:    lifecycleTestRedisAddr(),
		RedisDB:      lifecycleTestRedisDB,
		DisableStats: true, DisableHygiene: true, DisableErrorIndex: true, DisableJobs: true,
	}

	listening := make(chan string, 1)
	runErr := make(chan error, 1)
	go func() { runErr <- run(ctx, cfg, func(a net.Addr) { listening <- a.String() }) }()

	var addr string
	select {
	case addr = <-listening:
	case err := <-runErr:
		t.Fatalf("run returned before listening: %v", err)
	case <-time.After(10 * time.Second):
		t.Fatal("run never reported a listener")
	}

	get := func(path string) (int, http.Header) {
		resp, err := http.Get("http://" + addr + path)
		if err != nil {
			t.Fatalf("GET %s: %v", path, err)
		}
		defer resp.Body.Close()
		io.Copy(io.Discard, resp.Body)
		return resp.StatusCode, resp.Header
	}
	rootStatus, rootHdr := get("/")
	apiStatus, apiHdr := get("/api/queues")

	cancel()
	<-runErr

	Convey("Given the running asynqmon binary (#34)", t, func() {
		for name, h := range map[string]http.Header{"the SPA root": rootHdr, "the queues API": apiHdr} {
			Convey("When "+name+" is requested", func() {
				Convey("Then the response carries every security header", func() {
					So(h.Get("X-Content-Type-Options"), ShouldEqual, "nosniff")
					So(h.Get("X-Frame-Options"), ShouldEqual, "DENY")
					So(h.Get("Referrer-Policy"), ShouldEqual, "same-origin")
					So(h.Get("Content-Security-Policy"), ShouldContainSubstring, "frame-ancestors 'none'")
				})
			})
		}
		Convey("When both routes answer", func() {
			Convey("Then they serve their normal responses", func() {
				So(rootStatus, ShouldEqual, http.StatusOK)
				So(apiStatus, ShouldEqual, http.StatusOK)
			})
		})
		Convey("When the SPA root is served", func() {
			Convey("Then its CSP names the per-response script nonce", func() {
				So(rootHdr.Get("Content-Security-Policy"), ShouldContainSubstring, "script-src 'self' 'nonce-")
			})
		})
	})
}

package main

import (
	"crypto/tls"
	"strings"
	"testing"
	"time"

	"github.com/google/go-cmp/cmp"
	"github.com/google/go-cmp/cmp/cmpopts"
	"github.com/hibiken/asynq"
	. "github.com/smartystreets/goconvey/convey"
)

func TestParseFlags(t *testing.T) {
	tests := []struct {
		args []string
		want *Config
	}{
		{
			args: []string{"--redis-addr", "localhost:6380", "--redis-db", "3"},
			want: &Config{
				RedisAddr: "localhost:6380",
				RedisDB:   3,

				// Default values
				Port:                  8080,
				RedisPassword:         "",
				RedisUsername:         "", // upstream #273
				RedisSentinelPassword: "", // upstream #349
				RedisTLS:              "",
				RedisURL:              "",
				RedisInsecureTLS:      false,
				RedisClusterNodes:     "",
				MaxPayloadLength:      200,
				MaxResultLength:       200,
				// Detail endpoint safety cap (upstream #301): 256Ki chars.
				MaxDetailPayloadLength: 262144,
				// Task-scan limits (review issue #27).
				MaxConcurrentScans:    4,
				MaxScanCeiling:        20000,
				EnableMetricsExporter: false,
				PrometheusServerAddr:  "",
				PrometheusBasicAuth:   "", // upstream #248
				ReadOnly:              false,
				StatsInterval:         5 * time.Second,
				DisableStats:          false,
				CorrelationKeys:       "trace_id,correlation_id,request_id",
				MaxSSEConnections:     256,

				// Redis socket budget (#39) and the library-only options
				// that gained flags (#43).
				RedisTimeout:                 2 * time.Second,
				AttentionGroupStallAfter:     5 * time.Minute,
				AttentionPausedLongAfter:     7 * 24 * time.Hour,
				AttentionPendingAgeSLO:       5 * time.Minute,
				AttentionRetryStormThreshold: 1000,
				JobConcurrency:               2,

				Args: []string{},
			},
		},
	}

	for _, tc := range tests {
		t.Run(strings.Join(tc.args, " "), func(t *testing.T) {
			cfg, output, err := parseFlags("asynqmon", tc.args)
			if err != nil {
				t.Errorf("parseFlags returned error: %v", err)
			}
			if output != "" {
				t.Errorf("parseFlag returned output=%q, want empty", output)
			}
			if diff := cmp.Diff(tc.want, cfg); diff != "" {
				t.Errorf("parseFlag returned Config %v, want %v; (-want,+got)\n%s", cfg, tc.want, diff)
			}
		})
	}

}

// Flow-view correlation keys (Fleet Console §3.5): --correlation-keys /
// CORRELATION_KEYS, comma-separated, flag wins over env. Parsing runs
// imperatively before the Convey tree (the repo's goconvey discipline);
// the tree only reads captured results.
func TestCorrelationKeysFlagParsing(t *testing.T) {
	defCfg, defOut, defErr := parseFlags("asynqmon", []string{})

	flagCfg, flagOut, flagErr := parseFlags("asynqmon",
		[]string{"--correlation-keys", "order_ref,trace_id"})

	t.Setenv("CORRELATION_KEYS", "batch_id,run_id")
	envCfg, envOut, envErr := parseFlags("asynqmon", []string{})
	bothCfg, bothOut, bothErr := parseFlags("asynqmon",
		[]string{"--correlation-keys", "order_ref"})

	Convey("Given the asynqmon command-line flag set", t, func() {
		Convey("When no correlation flag or env is provided", func() {
			Convey("Then the default recognized keys apply", func() {
				So(defErr, ShouldBeNil)
				So(defOut, ShouldBeEmpty)
				So(defCfg.CorrelationKeys, ShouldEqual, "trace_id,correlation_id,request_id")
			})
		})
		Convey("When --correlation-keys is passed", func() {
			Convey("Then the comma-separated list replaces the default", func() {
				So(flagErr, ShouldBeNil)
				So(flagOut, ShouldBeEmpty)
				So(flagCfg.CorrelationKeys, ShouldEqual, "order_ref,trace_id")
			})
		})
		Convey("When only the CORRELATION_KEYS env var is set", func() {
			Convey("Then the env value applies", func() {
				So(envErr, ShouldBeNil)
				So(envOut, ShouldBeEmpty)
				So(envCfg.CorrelationKeys, ShouldEqual, "batch_id,run_id")
			})
		})
		Convey("When both the flag and the env var are set", func() {
			Convey("Then the flag wins", func() {
				So(bothErr, ShouldBeNil)
				So(bothOut, ShouldBeEmpty)
				So(bothCfg.CorrelationKeys, ShouldEqual, "order_ref")
			})
		})
	})
}

// Upstream-issue flags: --redis-username / REDIS_USERNAME (#273),
// --redis-sentinel-password / REDIS_SENTINEL_PASSWORD (#349), and
// --prometheus-basic-auth / PROMETHEUS_BASIC_AUTH (#248). Parsing runs
// imperatively before the Convey tree (the repo's goconvey discipline);
// the tree only reads captured results.
func TestUpstreamAuthFlagParsing(t *testing.T) {
	defCfg, defOut, defErr := parseFlags("asynqmon", []string{})

	flagCfg, flagOut, flagErr := parseFlags("asynqmon", []string{
		"--redis-username", "acl-user",
		"--redis-sentinel-password", "sentinel-secret",
		"--prometheus-basic-auth", "prom:pass",
	})

	t.Setenv("REDIS_USERNAME", "env-user")
	t.Setenv("REDIS_SENTINEL_PASSWORD", "env-sentinel")
	t.Setenv("PROMETHEUS_BASIC_AUTH", "env-prom:env-pass")
	envCfg, envOut, envErr := parseFlags("asynqmon", []string{})
	bothCfg, bothOut, bothErr := parseFlags("asynqmon",
		[]string{"--redis-username", "flag-user"})

	Convey("Given the asynqmon command-line flag set", t, func() {
		Convey("When no auth flag or env is provided", func() {
			Convey("Then all three default to empty", func() {
				So(defErr, ShouldBeNil)
				So(defOut, ShouldBeEmpty)
				So(defCfg.RedisUsername, ShouldBeEmpty)
				So(defCfg.RedisSentinelPassword, ShouldBeEmpty)
				So(defCfg.PrometheusBasicAuth, ShouldBeEmpty)
			})
		})
		Convey("When the flags are passed", func() {
			Convey("Then each value lands on its Config field", func() {
				So(flagErr, ShouldBeNil)
				So(flagOut, ShouldBeEmpty)
				So(flagCfg.RedisUsername, ShouldEqual, "acl-user")
				So(flagCfg.RedisSentinelPassword, ShouldEqual, "sentinel-secret")
				So(flagCfg.PrometheusBasicAuth, ShouldEqual, "prom:pass")
			})
		})
		Convey("When only the env vars are set", func() {
			Convey("Then the env values apply", func() {
				So(envErr, ShouldBeNil)
				So(envOut, ShouldBeEmpty)
				So(envCfg.RedisUsername, ShouldEqual, "env-user")
				So(envCfg.RedisSentinelPassword, ShouldEqual, "env-sentinel")
				So(envCfg.PrometheusBasicAuth, ShouldEqual, "env-prom:env-pass")
			})
		})
		Convey("When both a flag and its env var are set", func() {
			Convey("Then the flag wins and untouched flags keep their env values", func() {
				So(bothErr, ShouldBeNil)
				So(bothOut, ShouldBeEmpty)
				So(bothCfg.RedisUsername, ShouldEqual, "flag-user")
				So(bothCfg.RedisSentinelPassword, ShouldEqual, "env-sentinel")
			})
		})
	})
}

// makeRedisConnOpt wiring for the upstream auth issues: the ACL username
// (#273) must reach Username in single, cluster, and sentinel modes, and the
// sentinel password (#349) must reach SentinelPassword — with --redis-password
// authenticating to the redis servers behind the sentinels, not the sentinels
// themselves. Conversions run imperatively; the Convey tree only asserts.
func TestMakeRedisConnOptAuth(t *testing.T) {
	single, singleErr := makeRedisConnOpt(&Config{
		RedisAddr:     "localhost:6380",
		RedisUsername: "acl-user",
		RedisPassword: "secret",
	})

	viaURL, viaURLErr := makeRedisConnOpt(&Config{
		RedisURL:      "redis://:bar@localhost:6381/2",
		RedisUsername: "acl-user",
	})

	cluster, clusterErr := makeRedisConnOpt(&Config{
		RedisClusterNodes: "localhost:5000,localhost:5001",
		RedisUsername:     "acl-user",
		RedisPassword:     "secret",
	})

	sentinel, sentinelErr := makeRedisConnOpt(&Config{
		RedisURL:              "redis-sentinel://:url-sentinel-pass@localhost:5000?master=mymaster",
		RedisUsername:         "acl-user",
		RedisPassword:         "redis-pass",
		RedisSentinelPassword: "flag-sentinel-pass",
	})

	sentinelURLOnly, sentinelURLOnlyErr := makeRedisConnOpt(&Config{
		RedisURL: "redis-sentinel://:url-sentinel-pass@localhost:5000?master=mymaster",
	})

	Convey("Given redis connection configs carrying the upstream auth options", t, func() {
		Convey("When connecting to a single redis server by address", func() {
			Convey("Then the ACL username rides next to the password (#273)", func() {
				So(singleErr, ShouldBeNil)
				opt, ok := single.(asynq.RedisClientOpt)
				So(ok, ShouldBeTrue)
				So(opt.Username, ShouldEqual, "acl-user")
				So(opt.Password, ShouldEqual, "secret")
			})
		})
		Convey("When connecting via a redis:// URL", func() {
			Convey("Then the explicit username applies on top of the URL values", func() {
				So(viaURLErr, ShouldBeNil)
				opt, ok := viaURL.(asynq.RedisClientOpt)
				So(ok, ShouldBeTrue)
				So(opt.Username, ShouldEqual, "acl-user")
				So(opt.Password, ShouldEqual, "bar")
				So(opt.DB, ShouldEqual, 2)
			})
		})
		Convey("When connecting to a redis cluster", func() {
			Convey("Then the ACL username reaches the cluster options (#273)", func() {
				So(clusterErr, ShouldBeNil)
				opt, ok := cluster.(asynq.RedisClusterClientOpt)
				So(ok, ShouldBeTrue)
				So(opt.Username, ShouldEqual, "acl-user")
				So(opt.Password, ShouldEqual, "secret")
			})
		})
		Convey("When connecting via redis-sentinel with the sentinel-password flag", func() {
			Convey("Then the flag overrides the URL userinfo for the sentinels (#349)", func() {
				So(sentinelErr, ShouldBeNil)
				opt, ok := sentinel.(asynq.RedisFailoverClientOpt)
				So(ok, ShouldBeTrue)
				So(opt.SentinelPassword, ShouldEqual, "flag-sentinel-pass")

				Convey("And --redis-username/--redis-password authenticate to the servers behind them", func() {
					So(opt.Username, ShouldEqual, "acl-user")
					So(opt.Password, ShouldEqual, "redis-pass")
				})
			})
		})
		Convey("When connecting via redis-sentinel with only the URL userinfo", func() {
			Convey("Then the URL-provided sentinel password is preserved", func() {
				So(sentinelURLOnlyErr, ShouldBeNil)
				opt, ok := sentinelURLOnly.(asynq.RedisFailoverClientOpt)
				So(ok, ShouldBeTrue)
				So(opt.SentinelPassword, ShouldEqual, "url-sentinel-pass")
				So(opt.Password, ShouldBeEmpty)
				So(opt.Username, ShouldBeEmpty)
			})
		})
	})
}

func TestMakeRedisConnOpt(t *testing.T) {
	var tests = []struct {
		desc string
		cfg  *Config
		want asynq.RedisConnOpt
	}{
		{
			desc: "With address, db number and password",
			cfg: &Config{
				RedisAddr:     "localhost:6380",
				RedisDB:       1,
				RedisPassword: "foo",
			},
			want: asynq.RedisClientOpt{
				Addr:         "localhost:6380",
				DB:           1,
				Password:     "foo",
				DialTimeout:  2 * time.Second,
				ReadTimeout:  2 * time.Second,
				WriteTimeout: 2 * time.Second,
				PoolSize:     20,
			},
		},
		{
			desc: "With TLS server name",
			cfg: &Config{
				RedisAddr: "localhost:6379",
				RedisTLS:  "foobar",
			},
			want: asynq.RedisClientOpt{
				Addr:         "localhost:6379",
				TLSConfig:    &tls.Config{ServerName: "foobar"},
				DialTimeout:  2 * time.Second,
				ReadTimeout:  2 * time.Second,
				WriteTimeout: 2 * time.Second,
				PoolSize:     20,
			},
		},
		{
			desc: "With redis URL",
			cfg: &Config{
				RedisURL: "redis://:bar@localhost:6381/2",
			},
			want: asynq.RedisClientOpt{
				Addr:         "localhost:6381",
				DB:           2,
				Password:     "bar",
				DialTimeout:  2 * time.Second,
				ReadTimeout:  2 * time.Second,
				WriteTimeout: 2 * time.Second,
				PoolSize:     20,
			},
		},
		{
			desc: "With redis-sentinel URL",
			cfg: &Config{
				RedisURL: "redis-sentinel://:secretpassword@localhost:5000,localhost:5001,localhost:5002?master=mymaster",
			},
			want: asynq.RedisFailoverClientOpt{
				MasterName: "mymaster",
				SentinelAddrs: []string{
					"localhost:5000", "localhost:5001", "localhost:5002"},
				// The userinfo password in a redis-sentinel:// URL authenticates
				// to the sentinel nodes, so asynq maps it to SentinelPassword.
				SentinelPassword: "secretpassword",
				DialTimeout:      2 * time.Second,
				ReadTimeout:      2 * time.Second,
				WriteTimeout:     2 * time.Second,
				PoolSize:         20,
			},
		},
		{
			desc: "With cluster nodes",
			cfg: &Config{
				RedisClusterNodes: "localhost:5000,localhost:5001,localhost:5002,localhost:5003,localhost:5004,localhost:5005",
			},
			want: asynq.RedisClusterClientOpt{
				Addrs: []string{
					"localhost:5000", "localhost:5001", "localhost:5002", "localhost:5003", "localhost:5004", "localhost:5005"},
				DialTimeout:  2 * time.Second,
				ReadTimeout:  2 * time.Second,
				WriteTimeout: 2 * time.Second,
			},
		},
	}

	for _, tc := range tests {
		t.Run(tc.desc, func(t *testing.T) {
			// The configs above leave RedisTimeout zero, so the wants
			// above also assert the defaultRedisTimeout fallback.
			got, err := makeRedisConnOpt(tc.cfg)
			if err != nil {
				t.Fatalf("makeRedisConnOpt returned error: %v", err)
			}

			if diff := cmp.Diff(tc.want, got, cmpopts.IgnoreUnexported(tls.Config{})); diff != "" {
				t.Errorf("diff found: want=%v, got=%v; (-want,+got)\n%s",
					tc.want, got, diff)
			}
		})
	}
}

// Library-only options that gained flags and env vars (#43): the background
// role switches, the bulk-job concurrency, the hygiene webhook, and the four
// attention thresholds. Parsing runs imperatively before the Convey tree
// (the repo's goconvey discipline); the tree only reads captured results.
func TestBackgroundRoleFlagParsing(t *testing.T) {
	defCfg, _, defErr := parseFlags("asynqmon", []string{})

	flagCfg, flagOut, flagErr := parseFlags("asynqmon", []string{
		"--job-concurrency", "8",
		"--disable-jobs",
		"--disable-error-index",
		"--disable-hygiene",
		"--hygiene-webhook-url", "https://hooks.example/asynqmon",
		"--attention-pending-age-slo", "90s",
		"--attention-retry-storm-threshold", "250",
		"--attention-paused-long-after", "48h",
		"--attention-group-stall-after", "3m",
		"--redis-timeout", "750ms",
	})

	t.Setenv("JOB_CONCURRENCY", "5")
	t.Setenv("DISABLE_JOBS", "true")
	t.Setenv("DISABLE_ERROR_INDEX", "true")
	t.Setenv("DISABLE_HYGIENE", "true")
	t.Setenv("HYGIENE_WEBHOOK_URL", "https://env.example/hook")
	t.Setenv("ATTENTION_PENDING_AGE_SLO", "11m")
	t.Setenv("ATTENTION_RETRY_STORM_THRESHOLD", "77")
	t.Setenv("ATTENTION_PAUSED_LONG_AFTER", "72h")
	t.Setenv("ATTENTION_GROUP_STALL_AFTER", "12m")
	t.Setenv("REDIS_TIMEOUT", "4s")
	envCfg, envOut, envErr := parseFlags("asynqmon", []string{})
	bothCfg, _, bothErr := parseFlags("asynqmon", []string{"--job-concurrency", "3"})

	Convey("Given the flags for the library-only options (#43)", t, func() {
		Convey("When nothing is provided", func() {
			Convey("Then the library defaults apply", func() {
				So(defErr, ShouldBeNil)
				So(defCfg.JobConcurrency, ShouldEqual, 2)
				So(defCfg.DisableJobs, ShouldBeFalse)
				So(defCfg.DisableErrorIndex, ShouldBeFalse)
				So(defCfg.DisableHygiene, ShouldBeFalse)
				So(defCfg.HygieneWebhookURL, ShouldBeEmpty)
				So(defCfg.AttentionPendingAgeSLO, ShouldEqual, 5*time.Minute)
				So(defCfg.AttentionRetryStormThreshold, ShouldEqual, 1000)
				So(defCfg.AttentionPausedLongAfter, ShouldEqual, 7*24*time.Hour)
				So(defCfg.AttentionGroupStallAfter, ShouldEqual, 5*time.Minute)
				So(defCfg.RedisTimeout, ShouldEqual, 2*time.Second)
			})
		})
		Convey("When the flags are passed", func() {
			Convey("Then each value lands on its Config field", func() {
				So(flagErr, ShouldBeNil)
				So(flagOut, ShouldBeEmpty)
				So(flagCfg.JobConcurrency, ShouldEqual, 8)
				So(flagCfg.DisableJobs, ShouldBeTrue)
				So(flagCfg.DisableErrorIndex, ShouldBeTrue)
				So(flagCfg.DisableHygiene, ShouldBeTrue)
				So(flagCfg.HygieneWebhookURL, ShouldEqual, "https://hooks.example/asynqmon")
				So(flagCfg.AttentionPendingAgeSLO, ShouldEqual, 90*time.Second)
				So(flagCfg.AttentionRetryStormThreshold, ShouldEqual, 250)
				So(flagCfg.AttentionPausedLongAfter, ShouldEqual, 48*time.Hour)
				So(flagCfg.AttentionGroupStallAfter, ShouldEqual, 3*time.Minute)
				So(flagCfg.RedisTimeout, ShouldEqual, 750*time.Millisecond)
			})
		})
		Convey("When only the env vars are set", func() {
			Convey("Then the env values apply", func() {
				So(envErr, ShouldBeNil)
				So(envOut, ShouldBeEmpty)
				So(envCfg.JobConcurrency, ShouldEqual, 5)
				So(envCfg.DisableJobs, ShouldBeTrue)
				So(envCfg.DisableErrorIndex, ShouldBeTrue)
				So(envCfg.DisableHygiene, ShouldBeTrue)
				So(envCfg.HygieneWebhookURL, ShouldEqual, "https://env.example/hook")
				So(envCfg.AttentionPendingAgeSLO, ShouldEqual, 11*time.Minute)
				So(envCfg.AttentionRetryStormThreshold, ShouldEqual, 77)
				So(envCfg.AttentionPausedLongAfter, ShouldEqual, 72*time.Hour)
				So(envCfg.AttentionGroupStallAfter, ShouldEqual, 12*time.Minute)
				So(envCfg.RedisTimeout, ShouldEqual, 4*time.Second)
			})
		})
		Convey("When both a flag and its env var are set", func() {
			Convey("Then the flag wins and untouched flags keep their env values", func() {
				So(bothErr, ShouldBeNil)
				So(bothCfg.JobConcurrency, ShouldEqual, 3)
				So(bothCfg.HygieneWebhookURL, ShouldEqual, "https://env.example/hook")
			})
		})
	})
}

// Every flag value reaches asynqmon.Options (#43): a flag with no wiring is
// as useless as a missing flag.
func TestBuildOptionsCarriesEveryFlag(t *testing.T) {
	cfg := &Config{
		RedisAddr:                    "127.0.0.1:6379",
		ReadOnly:                     true,
		MaxDetailPayloadLength:       1234,
		PrometheusServerAddr:         "http://prom:9090",
		PrometheusBasicAuth:          "u:p",
		StatsInterval:                7 * time.Second,
		AuthHeader:                   "X-Auth-Request-User",
		TrustedProxies:               "10.0.0.0/8,192.168.0.0/16",
		RequireIdentity:              true,
		EnableEnqueue:                true,
		CorrelationKeys:              "job_id,trace_id",
		JobConcurrency:               6,
		DisableJobs:                  true,
		DisableErrorIndex:            true,
		DisableHygiene:               true,
		HygieneWebhookURL:            "https://hooks.example/h",
		AttentionPendingAgeSLO:       90 * time.Second,
		AttentionRetryStormThreshold: 250,
		AttentionPausedLongAfter:     48 * time.Hour,
		AttentionGroupStallAfter:     3 * time.Minute,
	}
	opts := buildOptions(cfg, asynq.RedisClientOpt{Addr: cfg.RedisAddr})

	Convey("Given a fully populated command-line config (#43)", t, func() {
		Convey("When it is converted into asynqmon.Options", func() {
			Convey("Then the background-role knobs are carried through", func() {
				So(opts.JobConcurrency, ShouldEqual, 6)
				So(opts.JobsDisabled, ShouldBeTrue)
				So(opts.ErrorIndexDisabled, ShouldBeTrue)
				So(opts.HygieneDisabled, ShouldBeTrue)
				So(opts.HygieneWebhookURL, ShouldEqual, "https://hooks.example/h")
			})
			Convey("Then the attention thresholds are carried through", func() {
				So(opts.AttentionPendingAgeSLO, ShouldEqual, 90*time.Second)
				So(opts.AttentionRetryStormThreshold, ShouldEqual, 250)
				So(opts.AttentionPausedLongAfter, ShouldEqual, 48*time.Hour)
				So(opts.AttentionGroupStallAfter, ShouldEqual, 3*time.Minute)
			})
			Convey("Then the identity and UI options are carried through", func() {
				So(opts.ReadOnly, ShouldBeTrue)
				So(opts.DetailPayloadLimit, ShouldEqual, 1234)
				So(opts.PrometheusAddress, ShouldEqual, "http://prom:9090")
				So(opts.PrometheusBasicAuth, ShouldEqual, "u:p")
				So(opts.StatsInterval, ShouldEqual, 7*time.Second)
				So(opts.AuthHeader, ShouldEqual, "X-Auth-Request-User")
				So(opts.TrustedProxies, ShouldResemble, []string{"10.0.0.0/8", "192.168.0.0/16"})
				So(opts.RequireIdentity, ShouldBeTrue)
				So(opts.EnableEnqueue, ShouldBeTrue)
				So(opts.CorrelationKeys, ShouldResemble, []string{"job_id", "trace_id"})
			})
		})
	})
}

// A REDIS_URL that carries no password must still authenticate with
// --redis-password / REDIS_PASSWORD (#43): the chart emits both when
// redis.url meets redis.existingSecret, and the old code dropped the
// password, failing at runtime with NOAUTH.
func TestRedisPasswordAppliesAfterURL(t *testing.T) {
	fromFlag, fromFlagErr := makeRedisConnOpt(&Config{
		RedisURL:      "redis://redis.example:6379/1",
		RedisPassword: "s3cret",
	})
	urlWins, urlWinsErr := makeRedisConnOpt(&Config{
		RedisURL:      "redis://:in-url@redis.example:6379/1",
		RedisPassword: "flag-pass",
	})
	sentinel, sentinelErr := makeRedisConnOpt(&Config{
		RedisURL:      "redis-sentinel://localhost:5000?master=mymaster",
		RedisPassword: "s3cret",
	})

	Convey("Given a redis URL carrying no password (#43)", t, func() {
		Convey("When --redis-password is set beside it", func() {
			Convey("Then the password is applied to the connection", func() {
				So(fromFlagErr, ShouldBeNil)
				opt, ok := fromFlag.(asynq.RedisClientOpt)
				So(ok, ShouldBeTrue)
				So(opt.Password, ShouldEqual, "s3cret")
				So(opt.DB, ShouldEqual, 1)
			})
		})
		Convey("When the URL does carry a password", func() {
			Convey("Then the URL keeps precedence", func() {
				So(urlWinsErr, ShouldBeNil)
				opt, ok := urlWins.(asynq.RedisClientOpt)
				So(ok, ShouldBeTrue)
				So(opt.Password, ShouldEqual, "in-url")
			})
		})
		Convey("When the URL is a sentinel URL", func() {
			Convey("Then the password authenticates to the servers behind it", func() {
				So(sentinelErr, ShouldBeNil)
				opt, ok := sentinel.(asynq.RedisFailoverClientOpt)
				So(ok, ShouldBeTrue)
				So(opt.Password, ShouldEqual, "s3cret")
			})
		})
	})
}

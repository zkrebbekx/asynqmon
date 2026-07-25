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
				EnableMetricsExporter:  false,
				PrometheusServerAddr:   "",
				PrometheusBasicAuth:    "", // upstream #248
				ReadOnly:               false,
				StatsInterval:          5 * time.Second,
				DisableStats:           false,
				CorrelationKeys:        "trace_id,correlation_id,request_id",

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
				Addr:     "localhost:6380",
				DB:       1,
				Password: "foo",
			},
		},
		{
			desc: "With TLS server name",
			cfg: &Config{
				RedisAddr: "localhost:6379",
				RedisTLS:  "foobar",
			},
			want: asynq.RedisClientOpt{
				Addr:      "localhost:6379",
				TLSConfig: &tls.Config{ServerName: "foobar"},
			},
		},
		{
			desc: "With redis URL",
			cfg: &Config{
				RedisURL: "redis://:bar@localhost:6381/2",
			},
			want: asynq.RedisClientOpt{
				Addr:     "localhost:6381",
				DB:       2,
				Password: "bar",
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
			},
		},
	}

	for _, tc := range tests {
		t.Run(tc.desc, func(t *testing.T) {
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

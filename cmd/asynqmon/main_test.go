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
				RedisTLS:              "",
				RedisURL:              "",
				RedisInsecureTLS:      false,
				RedisClusterNodes:     "",
				MaxPayloadLength:      200,
				MaxResultLength:       200,
				EnableMetricsExporter: false,
				PrometheusServerAddr:  "",
				ReadOnly:              false,
				StatsInterval:         5 * time.Second,
				DisableStats:          false,
				CorrelationKeys:       "trace_id,correlation_id,request_id",

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

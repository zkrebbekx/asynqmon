package main

import (
	"bytes"
	"context"
	"crypto/tls"
	"errors"
	"flag"
	"fmt"
	"log"
	"net"
	"net/http"
	"net/url"
	"os"
	"os/signal"
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/hibiken/asynq"
	"github.com/hibiken/asynq/x/metrics"
	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/promhttp"
	"github.com/redis/go-redis/v9"
	"github.com/rs/cors"
	"github.com/zkrebbekx/asynqmon"
)

// version is the build version stamped at link time:
//
//	go build -ldflags "-X main.version=v0.9.0"
//
// It is printed by --version, logged at startup, and served by
// GET /api/features as "version". "devel" means an unstamped build.
var version = "devel"

// Lifecycle budgets.
const (
	// shutdownTimeout bounds srv.Shutdown on SIGINT/SIGTERM. In-flight
	// requests older than this are cut with srv.Close.
	shutdownTimeout = 10 * time.Second
	// startupProbeTimeout bounds the INFO server call at boot.
	startupProbeTimeout = 3 * time.Second
	// defaultRedisTimeout is the --redis-timeout default: dial, read, and
	// write. It must not exceed the 2s /healthz PING budget.
	defaultRedisTimeout = 2 * time.Second
	// redisPoolSize is the go-redis connection pool size per client.
	redisPoolSize = 20
)

// Config holds configurations for the program provided via the command line.
type Config struct {
	// Server port
	Port int

	// Redis connection options
	RedisAddr         string
	RedisDB           int
	RedisPassword     string
	RedisTLS          string
	RedisURL          string
	RedisInsecureTLS  bool
	RedisClusterNodes string

	// RedisTimeout is the dial, read, and write timeout of every Redis
	// connection. Default 2s. Bounded so a hung Redis fails a request
	// within the /healthz budget instead of the go-redis 3s default.
	RedisTimeout time.Duration

	// RedisUsername is the Redis 6 ACL username sent alongside RedisPassword
	// in single, cluster, and sentinel modes (upstream hibiken/asynqmon#273).
	RedisUsername string

	// RedisSentinelPassword authenticates to the sentinel nodes themselves;
	// the redis servers behind them keep using RedisPassword (upstream
	// hibiken/asynqmon#349). Env-backed so it stays out of the redis-sentinel
	// URL and process argv.
	RedisSentinelPassword string

	// UI related configs
	ReadOnly         bool
	MaxPayloadLength int
	MaxResultLength  int

	// MaxDetailPayloadLength caps the formatted payload/result served by the
	// task DETAIL endpoint (GET /api/queues/{qname}/tasks/{task_id}) in utf8
	// characters (upstream hibiken/asynqmon#301). Lists stay capped by
	// MaxPayloadLength/MaxResultLength; the detail view gets the full value
	// up to this safety cap. 0 = unlimited. Default 262144.
	MaxDetailPayloadLength int

	// Task-scan limits (review issue #27): concurrent scans per process and
	// the ceiling on a request's max_scan.
	MaxConcurrentScans int
	MaxScanCeiling     int

	// Fleet stats sweeper configs
	StatsInterval time.Duration
	DisableStats  bool

	// Attention-engine detector thresholds (Options.Attention*). Zero
	// means the library default.
	AttentionGroupStallAfter     time.Duration
	AttentionPausedLongAfter     time.Duration
	AttentionPendingAgeSLO       time.Duration
	AttentionRetryStormThreshold int

	// Background-role switches and knobs for the error-signature indexer,
	// the hygiene scheduler, and the bulk-job runner.
	DisableErrorIndex bool
	DisableHygiene    bool
	DisableJobs       bool
	HygieneWebhookURL string
	JobConcurrency    int

	// Identity & audit configs (Fleet Console §5.11)
	AuthHeader      string
	TrustedProxies  string
	RequireIdentity bool
	// TrustBasicAuthUser accepts the Basic-Auth username as the actor.
	TrustBasicAuthUser bool
	// AllowUntrustedAuthHeader lets the binary start with AuthHeader set
	// and TrustedProxies empty (the header is then trusted from any peer).
	AllowUntrustedAuthHeader bool

	// MaxSSEConnections caps concurrent /api/fleet/events streams.
	MaxSSEConnections int

	// HygieneRunInReadOnly keeps POST /api/hygiene/{kind}/run available in
	// read-only mode.
	HygieneRunInReadOnly bool

	// Enqueue capability (Fleet Console §5.10). Default off; always
	// excluded in read-only mode.
	EnableEnqueue bool

	// Comma-separated payload keys the task drawer's Flow view recognizes
	// as correlation ids (Fleet Console §3.5), in priority order.
	CorrelationKeys string

	// Comma-separated list of origins allowed to make cross-origin requests.
	// Empty (the default) means same-origin only.
	CorsAllowedOrigins string

	// Prometheus related configs
	EnableMetricsExporter bool
	PrometheusServerAddr  string

	// PrometheusBasicAuth is "user:password" credentials attached to every
	// query proxied to PrometheusServerAddr (upstream hibiken/asynqmon#248).
	// Never logged.
	PrometheusBasicAuth string

	// PurgeOwnedKeys makes the binary delete every asynqmon:* key in the
	// configured Redis database and exit instead of serving.
	PurgeOwnedKeys bool

	// ShowVersion makes the binary print its version and exit.
	ShowVersion bool

	// Args are the positional (non-flag) command line arguments
	Args []string
}

// parseFlags parses the command-line arguments provided to the program.
// Typically, os.Args[0] is provided as 'progname' and os.args[1:] as 'args'.
// Returns the Config in case parsing succeeded, or an error. In any case, the
// output of the flag.Parse is returned in output.
//
// Reference: https://eli.thegreenplace.net/2020/testing-flag-parsing-in-go-programs/
func parseFlags(progname string, args []string) (cfg *Config, output string, err error) {
	flags := flag.NewFlagSet(progname, flag.ContinueOnError)
	var buf bytes.Buffer
	flags.SetOutput(&buf)

	var conf Config
	flags.IntVar(&conf.Port, "port", getEnvOrDefaultInt("PORT", 8080), "port number to use for web ui server")
	flags.StringVar(&conf.RedisAddr, "redis-addr", getEnvDefaultString("REDIS_ADDR", "127.0.0.1:6379"), "address of redis server to connect to")
	flags.IntVar(&conf.RedisDB, "redis-db", getEnvOrDefaultInt("REDIS_DB", 0), "redis database number")
	flags.StringVar(&conf.RedisPassword, "redis-password", getEnvDefaultString("REDIS_PASSWORD", ""), "password to use when connecting to redis server")
	flags.StringVar(&conf.RedisUsername, "redis-username", getEnvDefaultString("REDIS_USERNAME", ""), "redis ACL username sent alongside --redis-password in single, cluster, and sentinel modes (upstream #273; this is the plain redis AUTH username, distinct from any cloud-IAM --redis-user identity)")
	flags.StringVar(&conf.RedisSentinelPassword, "redis-sentinel-password", getEnvDefaultString("REDIS_SENTINEL_PASSWORD", ""), "password to authenticate to the sentinel nodes themselves; the redis servers behind them use --redis-password (upstream #349)")
	flags.StringVar(&conf.RedisTLS, "redis-tls", getEnvDefaultString("REDIS_TLS", ""), "server name for TLS validation used when connecting to redis server")
	flags.StringVar(&conf.RedisURL, "redis-url", getEnvDefaultString("REDIS_URL", ""), "URL to redis server")
	flags.BoolVar(&conf.RedisInsecureTLS, "redis-insecure-tls", getEnvOrDefaultBool("REDIS_INSECURE_TLS", false), "disable TLS certificate host checks")
	flags.StringVar(&conf.RedisClusterNodes, "redis-cluster-nodes", getEnvDefaultString("REDIS_CLUSTER_NODES", ""), "comma separated list of host:port addresses of cluster nodes")
	flags.DurationVar(&conf.RedisTimeout, "redis-timeout", getEnvOrDefaultDuration("REDIS_TIMEOUT", defaultRedisTimeout), "dial, read, and write timeout of every redis connection (e.g. 2s, 500ms)")
	flags.IntVar(&conf.MaxPayloadLength, "max-payload-length", getEnvOrDefaultInt("MAX_PAYLOAD_LENGTH", 200), "maximum number of utf8 characters printed in the payload cell in the Web UI")
	flags.IntVar(&conf.MaxResultLength, "max-result-length", getEnvOrDefaultInt("MAX_RESULT_LENGTH", 200), "maximum number of utf8 characters printed in the result cell in the Web UI")
	flags.IntVar(&conf.MaxDetailPayloadLength, "max-detail-payload-length", getEnvOrDefaultInt("MAX_DETAIL_PAYLOAD_LENGTH", 262144), "maximum number of utf8 characters of formatted payload/result served on the task DETAIL endpoint (upstream #301); list cells stay capped by --max-payload-length/--max-result-length; 0 = unlimited")
	flags.IntVar(&conf.MaxConcurrentScans, "max-concurrent-scans", getEnvOrDefaultInt("MAX_CONCURRENT_SCANS", 4), "maximum number of task scans (GET /api/tasks, /api/task_metadata, /api/task_aggregate, POST /api/tasks:batch_filtered) this process runs at once; further scans get 429")
	flags.IntVar(&conf.MaxScanCeiling, "max-scan-ceiling", getEnvOrDefaultInt("MAX_SCAN_CEILING", 20000), "largest max_scan a task-scan request can ask for; larger values clamp to it")
	flags.BoolVar(&conf.EnableMetricsExporter, "enable-metrics-exporter", getEnvOrDefaultBool("ENABLE_METRICS_EXPORTER", false), "enable prometheus metrics exporter to expose queue metrics")
	flags.StringVar(&conf.PrometheusServerAddr, "prometheus-addr", getEnvDefaultString("PROMETHEUS_ADDR", ""), "address of prometheus server to query time series")
	flags.StringVar(&conf.PrometheusBasicAuth, "prometheus-basic-auth", getEnvDefaultString("PROMETHEUS_BASIC_AUTH", ""), "user:password basic-auth credentials sent with every query to --prometheus-addr (upstream #248); prefer the env var to keep the secret out of argv")
	flags.BoolVar(&conf.ReadOnly, "read-only", getEnvOrDefaultBool("READ_ONLY", false), "restrict to read-only mode")
	flags.DurationVar(&conf.StatsInterval, "stats-interval", getEnvOrDefaultDuration("STATS_INTERVAL", 5*time.Second), "interval between stats sweeps (e.g. 5s, 30s)")
	flags.BoolVar(&conf.DisableStats, "disable-stats", getEnvOrDefaultBool("DISABLE_STATS", false), "disable the background fleet stats sweeper and /api/fleet endpoints")
	flags.DurationVar(&conf.AttentionGroupStallAfter, "attention-group-stall-after", getEnvOrDefaultDuration("ATTENTION_GROUP_STALL_AFTER", 5*time.Minute), "raise a GROUP_STALL finding when the oldest member of a group has aggregated longer than this")
	flags.DurationVar(&conf.AttentionPausedLongAfter, "attention-paused-long-after", getEnvOrDefaultDuration("ATTENTION_PAUSED_LONG_AFTER", 7*24*time.Hour), "raise a PAUSED_LONG finding when a queue has been paused longer than this")
	flags.DurationVar(&conf.AttentionPendingAgeSLO, "attention-pending-age-slo", getEnvOrDefaultDuration("ATTENTION_PENDING_AGE_SLO", 5*time.Minute), "raise a PENDING_AGE finding when a queue's oldest pending task has waited longer than this")
	flags.IntVar(&conf.AttentionRetryStormThreshold, "attention-retry-storm-threshold", getEnvOrDefaultInt("ATTENTION_RETRY_STORM_THRESHOLD", 1000), "raise a RETRY_STORM finding when at least this many retries fire within the next 5 minutes")
	flags.BoolVar(&conf.DisableErrorIndex, "disable-error-index", getEnvOrDefaultBool("DISABLE_ERROR_INDEX", false), "disable this replica's error-signature indexer (/api/errors still serves the shared index)")
	flags.BoolVar(&conf.DisableHygiene, "disable-hygiene", getEnvOrDefaultBool("DISABLE_HYGIENE", false), "disable this replica's scheduled hygiene reports (/api/hygiene still serves persisted reports)")
	flags.BoolVar(&conf.DisableJobs, "disable-jobs", getEnvOrDefaultBool("DISABLE_JOBS", false), "disable this replica's bulk-job runner (/api/jobs still works; another replica runs the jobs)")
	flags.StringVar(&conf.HygieneWebhookURL, "hygiene-webhook-url", getEnvDefaultString("HYGIENE_WEBHOOK_URL", ""), "URL that receives a POST of every generated hygiene report (best effort, one attempt)")
	flags.IntVar(&conf.JobConcurrency, "job-concurrency", getEnvOrDefaultInt("JOB_CONCURRENCY", 2), "maximum number of bulk jobs this replica works at once")
	flags.StringVar(&conf.AuthHeader, "auth-header", getEnvDefaultString("AUTH_HEADER", ""), "reverse-proxy header resolved as the acting user for the audit log (e.g. X-Auth-Request-User)")
	flags.StringVar(&conf.TrustedProxies, "trusted-proxies", getEnvDefaultString("TRUSTED_PROXIES", ""), "comma separated CIDRs the auth header is trusted from (empty: trusted from any peer)")
	flags.BoolVar(&conf.RequireIdentity, "require-identity", getEnvOrDefaultBool("REQUIRE_IDENTITY", false), "refuse mutating requests that carry no resolvable identity")
	flags.BoolVar(&conf.TrustBasicAuthUser, "trust-basic-auth-user", getEnvOrDefaultBool("TRUST_BASIC_AUTH_USER", false), "accept the HTTP Basic-Auth username as the acting user for the audit log; asynqmon never verifies the password, so set this only behind a proxy that does")
	flags.BoolVar(&conf.AllowUntrustedAuthHeader, "allow-untrusted-auth-header", getEnvOrDefaultBool("ALLOW_UNTRUSTED_AUTH_HEADER", false), "start with --auth-header set and --trusted-proxies empty; the header is then trusted from every peer (insecure)")
	flags.IntVar(&conf.MaxSSEConnections, "max-sse-connections", getEnvOrDefaultInt("MAX_SSE_CONNECTIONS", 256), "maximum concurrent /api/fleet/events streams per replica; extra subscribers get 503 with Retry-After (negative: unlimited)")
	flags.BoolVar(&conf.HygieneRunInReadOnly, "hygiene-run-in-read-only", getEnvOrDefaultBool("HYGIENE_RUN_IN_READ_ONLY", false), "keep POST /api/hygiene/{kind}/run available in --read-only mode (default: blocked like every other mutation)")
	flags.BoolVar(&conf.EnableEnqueue, "enable-enqueue", getEnvOrDefaultBool("ENABLE_ENQUEUE", false), "enable creating tasks from the web ui (POST /api/queues/{qname}/tasks); always excluded in read-only mode")
	flags.StringVar(&conf.CorrelationKeys, "correlation-keys", getEnvDefaultString("CORRELATION_KEYS", "trace_id,correlation_id,request_id"), "comma separated list of payload keys the task drawer's Flow view recognizes as correlation ids, in priority order")
	flags.StringVar(&conf.CorsAllowedOrigins, "cors-allowed-origins", getEnvDefaultString("CORS_ALLOWED_ORIGINS", ""), "comma separated list of origins allowed to make cross-origin requests (default: same-origin only)")
	flags.BoolVar(&conf.PurgeOwnedKeys, "purge-owned-keys", getEnvOrDefaultBool("PURGE_OWNED_KEYS", false), "delete every asynqmon:* key in the configured redis database, print the counts, and exit (rollback helper; never touches asynq:* keys)")
	flags.BoolVar(&conf.ShowVersion, "version", false, "print the asynqmon version and exit")

	err = flags.Parse(args)
	if err != nil {
		return nil, buf.String(), err
	}
	conf.Args = flags.Args()
	return &conf, buf.String(), nil
}

// validateAuthHeaderTrust returns the fatal startup message for an
// --auth-header that would be trusted from every peer, or "" when the
// configuration is acceptable. --allow-untrusted-auth-header downgrades the
// refusal to the library's warning.
func validateAuthHeaderTrust(cfg *Config) string {
	if cfg.AuthHeader == "" || strings.TrimSpace(cfg.TrustedProxies) != "" || cfg.AllowUntrustedAuthHeader {
		return ""
	}
	return "--auth-header needs --trusted-proxies: without it the identity header is " +
		"trusted from every peer, so any direct client can forge the audit actor" +
		requireIdentityNote(cfg) +
		". Set --trusted-proxies to your reverse proxy's CIDRs (e.g. --trusted-proxies=10.0.0.0/8), " +
		"or pass --allow-untrusted-auth-header to accept the risk."
}

func requireIdentityNote(cfg *Config) string {
	if cfg.RequireIdentity {
		return " and still satisfy --require-identity"
	}
	return ""
}

func makeTLSConfig(cfg *Config) *tls.Config {
	if cfg.RedisTLS == "" && !cfg.RedisInsecureTLS {
		return nil
	}
	return &tls.Config{
		ServerName:         cfg.RedisTLS,
		InsecureSkipVerify: cfg.RedisInsecureTLS,
	}
}

// redisTimeout returns the configured per-connection timeout, or the
// default when the config carries none.
func redisTimeout(cfg *Config) time.Duration {
	if cfg.RedisTimeout <= 0 {
		return defaultRedisTimeout
	}
	return cfg.RedisTimeout
}

// makeRedisConnOpt converts the config into an asynq.RedisConnOpt. Every
// opt type gets DialTimeout, ReadTimeout, and WriteTimeout from
// --redis-timeout and a fixed PoolSize, so a hung Redis fails a request
// within the /healthz budget instead of the go-redis defaults.
// redactURIError replaces the password of a redis URI with "***" in an error
// message. asynq and go-redis echo the URI they failed to parse, and a
// redis://user:secret@host URI would otherwise reach the log through
// log.Fatal.
func redactURIError(err error, raw string) error {
	if err == nil {
		return nil
	}
	msg := err.Error()
	if u, perr := url.Parse(raw); perr == nil && u.User != nil {
		if secret, ok := u.User.Password(); ok && secret != "" {
			msg = strings.ReplaceAll(msg, secret, "***")
		}
	}
	return errors.New(msg)
}

func makeRedisConnOpt(cfg *Config) (asynq.RedisConnOpt, error) {
	timeout := redisTimeout(cfg)

	// Connecting to redis-cluster
	if len(cfg.RedisClusterNodes) > 0 {
		return asynq.RedisClusterClientOpt{
			Addrs:        strings.Split(cfg.RedisClusterNodes, ","),
			Username:     cfg.RedisUsername, // ACL username (upstream #273)
			Password:     cfg.RedisPassword,
			TLSConfig:    makeTLSConfig(cfg),
			DialTimeout:  timeout,
			ReadTimeout:  timeout,
			WriteTimeout: timeout,
			// asynq.RedisClusterClientOpt carries no PoolSize field; the
			// cluster client keeps the go-redis per-node default.
		}, nil
	}

	// Connecting to redis-sentinels
	if strings.HasPrefix(cfg.RedisURL, "redis-sentinel") {
		res, err := asynq.ParseRedisURI(cfg.RedisURL)
		if err != nil {
			return nil, redactURIError(err, cfg.RedisURL)
		}
		connOpt := res.(asynq.RedisFailoverClientOpt) // safe to type-assert
		// The userinfo password in a redis-sentinel:// URL authenticates to
		// the sentinel nodes. --redis-sentinel-password / REDIS_SENTINEL_PASSWORD
		// takes precedence over it, keeping the secret out of the URL
		// (upstream hibiken/asynqmon#349).
		if cfg.RedisSentinelPassword != "" {
			connOpt.SentinelPassword = cfg.RedisSentinelPassword
		}
		// --redis-username / --redis-password authenticate to the redis
		// servers behind the sentinels — distinct from the sentinel password
		// above (upstream #349, #273).
		if cfg.RedisUsername != "" {
			connOpt.Username = cfg.RedisUsername
		}
		if cfg.RedisPassword != "" {
			connOpt.Password = cfg.RedisPassword
		}
		connOpt.TLSConfig = makeTLSConfig(cfg)
		connOpt.DialTimeout = timeout
		connOpt.ReadTimeout = timeout
		connOpt.WriteTimeout = timeout
		connOpt.PoolSize = redisPoolSize
		return connOpt, nil
	}

	// Connecting to single redis server
	var connOpt asynq.RedisClientOpt
	if len(cfg.RedisURL) > 0 {
		res, err := asynq.ParseRedisURI(cfg.RedisURL)
		if err != nil {
			return nil, redactURIError(err, cfg.RedisURL)
		}
		connOpt = res.(asynq.RedisClientOpt) // safe to type-assert
	} else {
		connOpt.Addr = cfg.RedisAddr
		connOpt.DB = cfg.RedisDB
		connOpt.Password = cfg.RedisPassword
	}
	// ACL username (upstream #273). The explicit flag/env wins over anything
	// a redis:// URL carried.
	if cfg.RedisUsername != "" {
		connOpt.Username = cfg.RedisUsername
	}
	// --redis-password / REDIS_PASSWORD applies when the URL carried no
	// password, so a secret-backed REDIS_PASSWORD next to a password-less
	// REDIS_URL authenticates instead of failing with NOAUTH. A password in
	// the URL keeps precedence.
	if connOpt.Password == "" && cfg.RedisPassword != "" {
		connOpt.Password = cfg.RedisPassword
	}
	if connOpt.TLSConfig == nil {
		connOpt.TLSConfig = makeTLSConfig(cfg)
	}
	connOpt.DialTimeout = timeout
	connOpt.ReadTimeout = timeout
	connOpt.WriteTimeout = timeout
	connOpt.PoolSize = redisPoolSize
	return connOpt, nil
}

// splitList splits a comma separated flag value. An empty value gives nil.
func splitList(s string) []string {
	if s == "" {
		return nil
	}
	return strings.Split(s, ",")
}

// clusterGateNotice is logged once when --redis-cluster-nodes forces the
// write features off.
const clusterGateNotice = "redis cluster: the console's write features (fleet stats, error index, hygiene reports, bulk jobs) are unavailable on Redis Cluster in this version; StatsDisabled, ErrorIndexDisabled, HygieneDisabled, and JobsDisabled are forced on"

// buildOptions converts the config into asynqmon.Options. When
// --redis-cluster-nodes is set, the background writers are forced off: their
// fenced Lua batches touch keys outside the declared KEYS, which Redis
// Cluster rejects. The gate logs clusterGateNotice once.
func buildOptions(cfg *Config, redisConnOpt asynq.RedisConnOpt) asynqmon.Options {
	opts := asynqmon.Options{
		RedisConnOpt:     redisConnOpt,
		PayloadFormatter: asynqmon.PayloadFormatterFunc(payloadFormatterFunc(cfg)),
		ResultFormatter:  asynqmon.ResultFormatterFunc(resultFormatterFunc(cfg)),
		// Task DETAIL endpoint serves the full formatted payload/result up
		// to --max-detail-payload-length; lists stay truncated (upstream
		// hibiken/asynqmon#301). The limit is surfaced via /api/features so
		// the drawer can label capped payloads honestly.
		DetailPayloadFormatter: asynqmon.PayloadFormatterFunc(detailPayloadFormatterFunc(cfg)),
		DetailResultFormatter:  asynqmon.ResultFormatterFunc(detailResultFormatterFunc(cfg)),
		DetailPayloadLimit:     cfg.MaxDetailPayloadLength,
		MaxConcurrentScans:     cfg.MaxConcurrentScans,
		MaxScanCeiling:         cfg.MaxScanCeiling,
		PrometheusAddress:      cfg.PrometheusServerAddr,
		// Basic-auth credentials for the Prometheus proxy (upstream #248).
		// Passed through verbatim and never logged.
		PrometheusBasicAuth:          cfg.PrometheusBasicAuth,
		ReadOnly:                     cfg.ReadOnly,
		Version:                      version,
		StatsInterval:                cfg.StatsInterval,
		StatsDisabled:                cfg.DisableStats,
		AttentionPendingAgeSLO:       cfg.AttentionPendingAgeSLO,
		AttentionRetryStormThreshold: cfg.AttentionRetryStormThreshold,
		AttentionPausedLongAfter:     cfg.AttentionPausedLongAfter,
		AttentionGroupStallAfter:     cfg.AttentionGroupStallAfter,
		AuthHeader:                   cfg.AuthHeader,
		// Whitespace and empty entries are normalized (and an all-empty
		// list falls back to the defaults) inside the asynqmon library.
		TrustedProxies:     splitList(cfg.TrustedProxies),
		RequireIdentity:    cfg.RequireIdentity,
		JobConcurrency:     cfg.JobConcurrency,
		JobsDisabled:       cfg.DisableJobs,
		ErrorIndexDisabled: cfg.DisableErrorIndex,
		EnableEnqueue:      cfg.EnableEnqueue,
		CorrelationKeys:    splitList(cfg.CorrelationKeys),
		HygieneWebhookURL:  cfg.HygieneWebhookURL,
		HygieneDisabled:    cfg.DisableHygiene,
		// Identity hardening (#32), SSE cap (#35), hygiene run-now gate
		// (#53.3). The library warns about an untrusted auth header instead
		// of refusing; the refusal lives in validateConfig.
		TrustBasicAuthUser:       cfg.TrustBasicAuthUser,
		AllowUntrustedAuthHeader: cfg.AllowUntrustedAuthHeader,
		MaxSSEConnections:        cfg.MaxSSEConnections,
		HygieneRunInReadOnly:     cfg.HygieneRunInReadOnly,
	}
	if cfg.RedisClusterNodes != "" {
		opts.StatsDisabled = true
		opts.ErrorIndexDisabled = true
		opts.HygieneDisabled = true
		opts.JobsDisabled = true
		log.Print(clusterGateNotice)
	}
	return opts
}

func main() {
	cfg, output, err := parseFlags(os.Args[0], os.Args[1:])
	if err == flag.ErrHelp {
		fmt.Println(output)
		os.Exit(2)
	} else if err != nil {
		fmt.Printf("error: %v\n", err)
		fmt.Println(output)
		os.Exit(1)
	}
	if cfg.ShowVersion {
		fmt.Printf("asynqmon %s\n", version)
		return
	}
	if err := validateConfig(cfg); err != nil {
		log.Fatal(err)
	}

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	if cfg.PurgeOwnedKeys {
		if err := runPurge(ctx, cfg, os.Stdout); err != nil {
			log.Fatal(err)
		}
		return
	}

	err = run(ctx, cfg, func(addr net.Addr) {
		fmt.Printf("Asynq Monitoring WebUI server %s is listening on %s\n", version, addr)
	})
	if err != nil {
		log.Fatal(err)
	}
}

// validateConfig rejects flag combinations the binary refuses to run with.
func validateConfig(cfg *Config) error {
	// With --auth-header set and no --trusted-proxies, the header is trusted
	// from EVERY peer, so any client that can reach the listener directly
	// can forge the audit actor (and, with --require-identity, satisfy the
	// identity requirement). The library keeps the permissive single-proxy
	// default with a loud warning; the binary refuses the misconfiguration
	// unless the operator acknowledges it with --allow-untrusted-auth-header.
	if msg := validateAuthHeaderTrust(cfg); msg != "" {
		return errors.New(msg)
	}
	if _, err := parseTrustedProxyCIDRs(splitList(cfg.TrustedProxies)); err != nil {
		return err
	}
	return nil
}

// runPurge implements --purge-owned-keys: it deletes every asynqmon:* key in
// the configured Redis database, prints the counts to out, and returns.
func runPurge(ctx context.Context, cfg *Config, out *os.File) error {
	redisConnOpt, err := makeRedisConnOpt(cfg)
	if err != nil {
		return err
	}
	rc, ok := redisConnOpt.MakeRedisClient().(redis.UniversalClient)
	if !ok {
		// The value is deliberately not formatted into the message: it
		// carries the redis password, and this error reaches log.Fatal.
		return errors.New("unsupported redis connection type")
	}
	defer rc.Close()
	res, err := purgeOwnedKeys(ctx, rc)
	if err != nil {
		return fmt.Errorf("purge refused: %w (deleted 0 keys)", err)
	}
	fmt.Fprintf(out, "purged %d asynqmon-owned keys (%d matched by SCAN)\n", res.Deleted, res.Matched)
	return nil
}

// run builds the handler and serves HTTP until ctx is canceled or the
// listener fails. onListen is called once with the bound address.
//
// Shutdown order on cancel:
//  1. srv.Shutdown with shutdownTimeout. The server's BaseContext is ctx,
//     so a long-lived SSE handler sees its request context canceled and
//     returns; Shutdown then completes as soon as in-flight requests end.
//  2. srv.Close when the budget is exhausted.
//  3. h.Close, which stops every background engine while the Redis client
//     is still open, so each Stop releases its lease at once instead of
//     leaving it to expire.
func run(ctx context.Context, cfg *Config, onListen func(net.Addr)) error {
	redisConnOpt, err := makeRedisConnOpt(cfg)
	if err != nil {
		return err
	}
	logRedisVersion(ctx, redisConnOpt)

	h := asynqmon.New(buildOptions(cfg, redisConnOpt))
	defer h.Close()

	allowedOrigins := splitList(cfg.CorsAllowedOrigins)
	trustedProxies, err := parseTrustedProxyCIDRs(splitList(cfg.TrustedProxies))
	if err != nil {
		return err
	}

	// Reject cross-origin mutations (CSRF protection). The previous behavior —
	// a CORS wrapper allowing every origin with POST/DELETE — let any web page
	// the operator visited fire mutating requests at a localhost/intranet
	// dashboard. The SPA is served same-origin, so CORS is only enabled when
	// origins are explicitly allowed via -cors-allowed-origins.
	var handler http.Handler = csrfProtection(allowedOrigins, trustedProxies)(h)
	if len(allowedOrigins) > 0 {
		c := cors.New(cors.Options{
			AllowedOrigins: allowedOrigins,
			AllowedMethods: []string{"GET", "POST", "PUT", "DELETE"},
		})
		handler = c.Handler(handler)
	}
	mux := http.NewServeMux()
	mux.Handle("/", handler)
	if cfg.EnableMetricsExporter {
		// Using NewPedanticRegistry here to test the implementation of Collectors and Metrics.
		reg := prometheus.NewPedanticRegistry()

		inspector := asynq.NewInspector(redisConnOpt)
		defer inspector.Close()

		reg.MustRegister(
			metrics.NewQueueMetricsCollector(inspector),
			// Add the standard process and go metrics to the registry
			prometheus.NewProcessCollector(prometheus.ProcessCollectorOpts{}),
			prometheus.NewGoCollector(),
		)
		mux.Handle("/metrics", promhttp.HandlerFor(reg, promhttp.HandlerOpts{}))
	}

	srv := &http.Server{
		Handler:      loggingMiddleware(securityHeaders(mux)),
		WriteTimeout: 10 * time.Second,
		ReadTimeout:  10 * time.Second,
		BaseContext:  func(net.Listener) context.Context { return ctx },
	}

	ln, err := net.Listen("tcp", fmt.Sprintf(":%d", cfg.Port))
	if err != nil {
		return err
	}
	if onListen != nil {
		onListen(ln.Addr())
	}

	errCh := make(chan error, 1)
	go func() { errCh <- srv.Serve(ln) }()

	select {
	case err := <-errCh:
		return err
	case <-ctx.Done():
	}
	log.Print("shutdown: signal received, draining HTTP")
	shCtx, cancel := context.WithTimeout(context.Background(), shutdownTimeout)
	defer cancel()
	if err := srv.Shutdown(shCtx); err != nil {
		log.Printf("shutdown: drain budget exhausted, closing connections: %v", err)
		_ = srv.Close()
	}
	<-errCh // Serve returns http.ErrServerClosed
	return nil
}

func payloadFormatterFunc(cfg *Config) func(string, []byte) string {
	return func(taskType string, payload []byte) string {
		payloadStr := asynqmon.SmartPayloadFormatter.FormatPayload(taskType, payload)
		return truncate(payloadStr, cfg.MaxPayloadLength)
	}
}

// detailPayloadFormatterFunc formats payloads for the task DETAIL endpoint
// (upstream hibiken/asynqmon#301): same smart formatting, but capped by
// --max-detail-payload-length (0 = unlimited) instead of the list-cell cap.
func detailPayloadFormatterFunc(cfg *Config) func(string, []byte) string {
	return func(taskType string, payload []byte) string {
		payloadStr := asynqmon.SmartPayloadFormatter.FormatPayload(taskType, payload)
		if cfg.MaxDetailPayloadLength <= 0 {
			return payloadStr
		}
		return truncate(payloadStr, cfg.MaxDetailPayloadLength)
	}
}

// detailResultFormatterFunc is detailPayloadFormatterFunc for task results
// (#301) — the same --max-detail-payload-length safety cap applies.
func detailResultFormatterFunc(cfg *Config) func(string, []byte) string {
	return func(taskType string, result []byte) string {
		resultStr := asynqmon.SmartResultFormatter.FormatResult(taskType, result)
		if cfg.MaxDetailPayloadLength <= 0 {
			return resultStr
		}
		return truncate(resultStr, cfg.MaxDetailPayloadLength)
	}
}

func resultFormatterFunc(cfg *Config) func(string, []byte) string {
	return func(taskType string, result []byte) string {
		resultStr := asynqmon.SmartResultFormatter.FormatResult(taskType, result)
		return truncate(resultStr, cfg.MaxResultLength)
	}
}

// truncates string s to limit length (in utf8).
func truncate(s string, limit int) string {
	i := 0
	for pos := range s {
		if i == limit {
			return s[:pos] + "…"
		}
		i++
	}
	return s
}

func getEnvDefaultString(key, def string) string {
	v := os.Getenv(key)
	if v == "" {
		return def
	}

	return v
}

func getEnvOrDefaultInt(key string, def int) int {
	v, err := strconv.Atoi(os.Getenv(key))
	if err != nil {
		return def
	}
	return v
}

func getEnvOrDefaultBool(key string, def bool) bool {
	v, err := strconv.ParseBool(os.Getenv(key))
	if err != nil {
		return def
	}
	return v
}

func getEnvOrDefaultDuration(key string, def time.Duration) time.Duration {
	v, err := time.ParseDuration(os.Getenv(key))
	if err != nil {
		return def
	}
	return v
}

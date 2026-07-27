package main

import (
	"bytes"
	"crypto/tls"
	"flag"
	"fmt"
	"log"
	"net/http"
	"os"
	"strconv"
	"strings"
	"time"

	"github.com/hibiken/asynq"
	"github.com/hibiken/asynq/x/metrics"
	"github.com/hibiken/asynqmon"
	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/promhttp"
	"github.com/rs/cors"
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

	// Fleet stats sweeper configs
	StatsInterval time.Duration
	DisableStats  bool

	// Identity & audit configs (Fleet Console §5.11)
	AuthHeader      string
	TrustedProxies  string
	RequireIdentity bool

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
	flags.IntVar(&conf.MaxPayloadLength, "max-payload-length", getEnvOrDefaultInt("MAX_PAYLOAD_LENGTH", 200), "maximum number of utf8 characters printed in the payload cell in the Web UI")
	flags.IntVar(&conf.MaxResultLength, "max-result-length", getEnvOrDefaultInt("MAX_RESULT_LENGTH", 200), "maximum number of utf8 characters printed in the result cell in the Web UI")
	flags.IntVar(&conf.MaxDetailPayloadLength, "max-detail-payload-length", getEnvOrDefaultInt("MAX_DETAIL_PAYLOAD_LENGTH", 262144), "maximum number of utf8 characters of formatted payload/result served on the task DETAIL endpoint (upstream #301); list cells stay capped by --max-payload-length/--max-result-length; 0 = unlimited")
	flags.BoolVar(&conf.EnableMetricsExporter, "enable-metrics-exporter", getEnvOrDefaultBool("ENABLE_METRICS_EXPORTER", false), "enable prometheus metrics exporter to expose queue metrics")
	flags.StringVar(&conf.PrometheusServerAddr, "prometheus-addr", getEnvDefaultString("PROMETHEUS_ADDR", ""), "address of prometheus server to query time series")
	flags.StringVar(&conf.PrometheusBasicAuth, "prometheus-basic-auth", getEnvDefaultString("PROMETHEUS_BASIC_AUTH", ""), "user:password basic-auth credentials sent with every query to --prometheus-addr (upstream #248); prefer the env var to keep the secret out of argv")
	flags.BoolVar(&conf.ReadOnly, "read-only", getEnvOrDefaultBool("READ_ONLY", false), "restrict to read-only mode")
	flags.DurationVar(&conf.StatsInterval, "stats-interval", getEnvOrDefaultDuration("STATS_INTERVAL", 5*time.Second), "interval between stats sweeps (e.g. 5s, 30s)")
	flags.BoolVar(&conf.DisableStats, "disable-stats", getEnvOrDefaultBool("DISABLE_STATS", false), "disable the background fleet stats sweeper and /api/fleet endpoints")
	flags.StringVar(&conf.AuthHeader, "auth-header", getEnvDefaultString("AUTH_HEADER", ""), "reverse-proxy header resolved as the acting user for the audit log (e.g. X-Auth-Request-User)")
	flags.StringVar(&conf.TrustedProxies, "trusted-proxies", getEnvDefaultString("TRUSTED_PROXIES", ""), "comma separated CIDRs the auth header is trusted from (empty: trusted from any peer)")
	flags.BoolVar(&conf.RequireIdentity, "require-identity", getEnvOrDefaultBool("REQUIRE_IDENTITY", false), "refuse mutating requests that carry no resolvable identity")
	flags.BoolVar(&conf.EnableEnqueue, "enable-enqueue", getEnvOrDefaultBool("ENABLE_ENQUEUE", false), "enable creating tasks from the web ui (POST /api/queues/{qname}/tasks); always excluded in read-only mode")
	flags.StringVar(&conf.CorrelationKeys, "correlation-keys", getEnvDefaultString("CORRELATION_KEYS", "trace_id,correlation_id,request_id"), "comma separated list of payload keys the task drawer's Flow view recognizes as correlation ids, in priority order")
	flags.StringVar(&conf.CorsAllowedOrigins, "cors-allowed-origins", getEnvDefaultString("CORS_ALLOWED_ORIGINS", ""), "comma separated list of origins allowed to make cross-origin requests (default: same-origin only)")

	err = flags.Parse(args)
	if err != nil {
		return nil, buf.String(), err
	}
	conf.Args = flags.Args()
	return &conf, buf.String(), nil
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

func makeRedisConnOpt(cfg *Config) (asynq.RedisConnOpt, error) {
	// Connecting to redis-cluster
	if len(cfg.RedisClusterNodes) > 0 {
		return asynq.RedisClusterClientOpt{
			Addrs:     strings.Split(cfg.RedisClusterNodes, ","),
			Username:  cfg.RedisUsername, // ACL username (upstream #273)
			Password:  cfg.RedisPassword,
			TLSConfig: makeTLSConfig(cfg),
		}, nil
	}

	// Connecting to redis-sentinels
	if strings.HasPrefix(cfg.RedisURL, "redis-sentinel") {
		res, err := asynq.ParseRedisURI(cfg.RedisURL)
		if err != nil {
			return nil, err
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
		return connOpt, nil
	}

	// Connecting to single redis server
	var connOpt asynq.RedisClientOpt
	if len(cfg.RedisURL) > 0 {
		res, err := asynq.ParseRedisURI(cfg.RedisURL)
		if err != nil {
			return nil, err
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
	if connOpt.TLSConfig == nil {
		connOpt.TLSConfig = makeTLSConfig(cfg)
	}
	return connOpt, nil
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

	redisConnOpt, err := makeRedisConnOpt(cfg)
	if err != nil {
		log.Fatal(err)
	}

	var trustedProxies []string
	if cfg.TrustedProxies != "" {
		trustedProxies = strings.Split(cfg.TrustedProxies, ",")
	}

	// Whitespace and empty entries are normalized (and an all-empty list
	// falls back to the defaults) inside the asynqmon library.
	var correlationKeys []string
	if cfg.CorrelationKeys != "" {
		correlationKeys = strings.Split(cfg.CorrelationKeys, ",")
	}

	h := asynqmon.New(asynqmon.Options{
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
		PrometheusAddress:      cfg.PrometheusServerAddr,
		// Basic-auth credentials for the Prometheus proxy (upstream #248).
		// Passed through verbatim and never logged.
		PrometheusBasicAuth: cfg.PrometheusBasicAuth,
		ReadOnly:            cfg.ReadOnly,
		StatsInterval:       cfg.StatsInterval,
		StatsDisabled:       cfg.DisableStats,
		AuthHeader:          cfg.AuthHeader,
		TrustedProxies:      trustedProxies,
		RequireIdentity:     cfg.RequireIdentity,
		EnableEnqueue:       cfg.EnableEnqueue,
		CorrelationKeys:     correlationKeys,
	})
	defer h.Close()

	var allowedOrigins []string
	if cfg.CorsAllowedOrigins != "" {
		allowedOrigins = strings.Split(cfg.CorsAllowedOrigins, ",")
	}

	// Reject cross-origin mutations (CSRF protection). The previous behavior —
	// a CORS wrapper allowing every origin with POST/DELETE — let any web page
	// the operator visited fire mutating requests at a localhost/intranet
	// dashboard. The SPA is served same-origin, so CORS is only enabled when
	// origins are explicitly allowed via -cors-allowed-origins.
	var handler http.Handler = csrfProtection(allowedOrigins)(h)
	if len(allowedOrigins) > 0 {
		c := cors.New(cors.Options{
			AllowedOrigins: allowedOrigins,
			AllowedMethods: []string{"GET", "POST", "DELETE"},
		})
		handler = c.Handler(handler)
	}
	mux := http.NewServeMux()
	mux.Handle("/", handler)
	if cfg.EnableMetricsExporter {
		// Using NewPedanticRegistry here to test the implementation of Collectors and Metrics.
		reg := prometheus.NewPedanticRegistry()

		inspector := asynq.NewInspector(redisConnOpt)

		reg.MustRegister(
			metrics.NewQueueMetricsCollector(inspector),
			// Add the standard process and go metrics to the registry
			prometheus.NewProcessCollector(prometheus.ProcessCollectorOpts{}),
			prometheus.NewGoCollector(),
		)
		mux.Handle("/metrics", promhttp.HandlerFor(reg, promhttp.HandlerOpts{}))
	}

	srv := &http.Server{
		Handler:      loggingMiddleware(mux),
		Addr:         fmt.Sprintf(":%d", cfg.Port),
		WriteTimeout: 10 * time.Second,
		ReadTimeout:  10 * time.Second,
	}

	fmt.Printf("Asynq Monitoring WebUI server is listening on port %d\n", cfg.Port)
	log.Fatal(srv.ListenAndServe())
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

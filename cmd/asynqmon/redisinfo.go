package main

import (
	"context"
	"log"
	"strconv"
	"strings"

	"github.com/hibiken/asynq"
	"github.com/redis/go-redis/v9"
)

// ****************************************************************************
// Startup Redis probe: run INFO server once, log redis_version, and warn when
// the server is older than the minimum asynqmon needs.
// ****************************************************************************

// Minimum Redis version. ZMSCORE (AQL and task search), streams (audit
// log), MEMORY USAGE and HSTRLEN (hygiene) need 6.2.
const (
	minRedisMajor = 6
	minRedisMinor = 2
)

// logRedisVersion runs INFO server with startupProbeTimeout, logs the
// redis_version, and warns when it is below 6.2. A failed probe is logged
// and does not stop the boot: the handler retries Redis on its own.
func logRedisVersion(ctx context.Context, connOpt asynq.RedisConnOpt) {
	rc, ok := connOpt.MakeRedisClient().(redis.UniversalClient)
	if !ok {
		return
	}
	defer rc.Close()
	ctx, cancel := context.WithTimeout(ctx, startupProbeTimeout)
	defer cancel()
	info, err := rc.Info(ctx, "server").Result()
	if err != nil {
		log.Printf("redis: INFO server failed at startup (the handler retries on its own): %v", err)
		return
	}
	v := parseRedisVersion(info)
	if v == "" {
		log.Print("redis: INFO server carried no redis_version")
		return
	}
	log.Printf("redis: server version %s", v)
	if redisVersionBelow(v, minRedisMajor, minRedisMinor) {
		log.Printf("WARNING: redis %s is below the minimum %d.%d; ZMSCORE, streams, MEMORY USAGE, and HSTRLEN are required", v, minRedisMajor, minRedisMinor)
	}
}

// parseRedisVersion extracts the redis_version field from an INFO reply.
// It returns "" when the field is absent.
func parseRedisVersion(info string) string {
	for _, line := range strings.Split(info, "\n") {
		line = strings.TrimSpace(line)
		if v, ok := strings.CutPrefix(line, "redis_version:"); ok {
			return strings.TrimSpace(v)
		}
	}
	return ""
}

// redisVersionBelow reports whether version (e.g. "6.0.16") is lower than
// major.minor. An unparsable version is treated as not below, so a vendor
// string never raises a false warning.
func redisVersionBelow(version string, major, minor int) bool {
	parts := strings.Split(version, ".")
	if len(parts) < 2 {
		return false
	}
	gotMajor, err := strconv.Atoi(parts[0])
	if err != nil {
		return false
	}
	gotMinor, err := strconv.Atoi(parts[1])
	if err != nil {
		return false
	}
	if gotMajor != major {
		return gotMajor < major
	}
	return gotMinor < minor
}

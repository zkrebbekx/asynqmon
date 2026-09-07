package leasefence

import "os"

// testRedisAddrFromEnv returns the address of the Redis the integration
// tests flush and use. Set ASYNQMON_TEST_REDIS_ADDR to point the suites at a
// non-default instance (for example one per parallel checkout). The default is
// the local instance CI starts.
func testRedisAddrFromEnv() string {
	if v := os.Getenv("ASYNQMON_TEST_REDIS_ADDR"); v != "" {
		return v
	}
	return "127.0.0.1:6379"
}

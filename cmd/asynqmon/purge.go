package main

import (
	"context"
	"fmt"
	"strings"

	"github.com/redis/go-redis/v9"
)

// ****************************************************************************
// --purge-owned-keys: the rollback helper. It deletes every key asynqmon
// writes (prefix "asynqmon:") from the configured Redis database and leaves
// every asynq:* queue key untouched.
// ****************************************************************************

const (
	// ownedKeyPrefix is the prefix of every key asynqmon writes.
	ownedKeyPrefix = "asynqmon:"
	// purgeScanCount is the COUNT hint of each SCAN page.
	purgeScanCount = 500
	// purgeUnlinkBatch is the number of keys per UNLINK call.
	purgeUnlinkBatch = 500
)

// purgeResult is the outcome of purgeOwnedKeys.
type purgeResult struct {
	// Matched is the number of keys SCAN returned for the pattern.
	Matched int
	// Deleted is the number of keys UNLINK removed.
	Deleted int
}

// purgeOwnedKeys runs SCAN MATCH asynqmon:* COUNT 500 to completion, checks
// that every matched key starts with "asynqmon:", and then UNLINKs the keys
// in batches. The check happens before the first delete: when any key fails
// it, the function returns an error and deletes nothing. On a cluster
// client, every master is scanned.
func purgeOwnedKeys(ctx context.Context, rc redis.UniversalClient) (purgeResult, error) {
	var res purgeResult
	keys, err := scanOwnedKeys(ctx, rc)
	if err != nil {
		return res, err
	}
	res.Matched = len(keys)
	for _, k := range keys {
		if !strings.HasPrefix(k, ownedKeyPrefix) {
			return res, fmt.Errorf("SCAN returned key %q outside the %q prefix", k, ownedKeyPrefix)
		}
	}
	for start := 0; start < len(keys); start += purgeUnlinkBatch {
		end := min(start+purgeUnlinkBatch, len(keys))
		n, err := rc.Unlink(ctx, keys[start:end]...).Result()
		res.Deleted += int(n)
		if err != nil {
			return res, err
		}
	}
	return res, nil
}

// scanOwnedKeys returns every key matching asynqmon:* in the database. A
// cluster client is scanned master by master.
func scanOwnedKeys(ctx context.Context, rc redis.UniversalClient) ([]string, error) {
	var keys []string
	collect := func(ctx context.Context, c redis.UniversalClient) error {
		iter := c.Scan(ctx, 0, ownedKeyPrefix+"*", purgeScanCount).Iterator()
		for iter.Next(ctx) {
			keys = append(keys, iter.Val())
		}
		return iter.Err()
	}
	if cc, ok := rc.(*redis.ClusterClient); ok {
		err := cc.ForEachMaster(ctx, func(ctx context.Context, c *redis.Client) error {
			return collect(ctx, c)
		})
		return keys, err
	}
	return keys, collect(ctx, rc)
}

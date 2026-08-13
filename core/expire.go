package core

import (
	"time"
)

const expireSampleSize int = 20

func hasExpired(k string) bool {
	exp, ok := expires[k]
	if !ok {
		return false
	}
	return exp <= uint64(time.Now().UnixMilli())
}

func removeExpiry(k string) {
	delete(expires, k)
}

func getExpiry(k string) (uint64, bool) {
	exp, ok := expires[k]
	return exp, ok
}

func expireSample() float32 {
	var limit int = expireSampleSize
	var expiredCount int = 0
	var sampledCount int = 0

	for key := range expires {
		limit--
		sampledCount++
		if hasExpired(key) {
			Del(key)
			expiredCount++
		}
		if limit == 0 {
			break
		}
	}

	if sampledCount == 0 {
		return 0
	}
	return float32(expiredCount) / float32(sampledCount)
}

// Deletes all the expired keys - the active way
// Sampling approach: https://redis.io/commands/expire/
func DeleteExpiredKeys() {
	for {
		frac := expireSample()
		// if the sample had less than 25% keys expired
		// we break the loop.
		if frac < 0.25 {
			break
		}
	}
}

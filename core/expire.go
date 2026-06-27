package core

import (
	"time"

	"github.com/shivam30303/diceDB/config"
)

func hasExpired(obj *Obj) bool {
	exp, ok := expires[obj]
	if !ok {
		return false
	}
	return exp <= uint64(time.Now().UnixMilli())
}

func getExpiry(obj *Obj) (uint64, bool) {
	exp, ok := expires[obj]
	return exp, ok
}

// TODO: Optimize
//   - Sampling
//   - Unnecessary iteration
func expireSample() float32 {
	var limit int = config.KeysLimit
	var expiredCount int = 0
	var sampledCount int = 0

	for key, obj := range store {
		limit--
		sampledCount++
		if hasExpired(obj) {
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

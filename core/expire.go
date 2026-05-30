package core

import (
	"log"
	"time"
)

// TODO: Optimize
//   - Sampling
//   - Unnecessary iteration
func expireSample() float64 {
	var limit int = 20
	var expiredCount int = 0

	for key, obj := range store {
		if obj.ExpiresAt != -1 {
			limit--
			if obj.ExpiresAt <= time.Now().Unix() {
				delete(store, key)
				expiredCount++
			}
		}

		// once we iterated to 20 keys that have some expiration set
		// we break the loop
		if limit == 0 {
			break
		}
	}
	return float64(expiredCount) / float64(limit)
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
	log.Println("deleted the expired but undeleted keys. total keys", len(store))
}

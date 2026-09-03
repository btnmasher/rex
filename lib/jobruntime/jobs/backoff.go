package jobs

import (
	"crypto/rand"
	"math"
	"math/big"
	"time"
)

const backoffExponentBase = 2

// NextBackoff returns exponential backoff with full jitter.
func NextBackoff(attempt int, base, maxDelay time.Duration) time.Duration {
	if attempt <= 0 {
		return base
	}
	pow := math.Pow(backoffExponentBase, float64(attempt))
	d := time.Duration(float64(base) * pow)
	d = min(d, maxDelay)
	limit := int64(d) + 1
	n, err := rand.Int(rand.Reader, big.NewInt(limit))
	if err != nil {
		return d
	}
	return time.Duration(n.Int64())
}

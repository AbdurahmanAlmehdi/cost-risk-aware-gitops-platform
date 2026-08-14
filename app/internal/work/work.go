// Package work performs the demo workload's actual computation.
//
// The job is CPU-bound on purpose. M4 attributes cost primarily from CPU and memory
// consumption, so the workload has to genuinely consume CPU for the cost figures on the
// dashboard to move in response to load. A sleep would produce a queue that drains and
// a cost line that never budges, which would quietly break the central demonstration.
package work

import (
	"context"
	"crypto/sha256"
	"encoding/binary"
	"time"
)

// Do burns CPU for approximately d, returning early if ctx is cancelled so a worker
// can shut down promptly during a scale-down or rolling update rather than being
// SIGKILLed mid-job.
//
// It returns the number of hash rounds completed, which keeps the compiler from
// eliminating the loop as dead code.
func Do(ctx context.Context, d time.Duration) uint64 {
	if d <= 0 {
		return 0
	}
	deadline := time.Now().Add(d)

	var rounds uint64
	buf := make([]byte, 8)
	digest := sha256.New()

	for {
		// Checking the clock and context on every iteration would cost more than the
		// work itself, so both are checked once per batch. The batch is small enough
		// that cancellation still feels immediate (sub-millisecond).
		for i := 0; i < 2048; i++ {
			binary.LittleEndian.PutUint64(buf, rounds)
			digest.Reset()
			digest.Write(buf)
			_ = digest.Sum(nil)
			rounds++
		}
		if time.Now().After(deadline) {
			return rounds
		}
		select {
		case <-ctx.Done():
			return rounds
		default:
		}
	}
}

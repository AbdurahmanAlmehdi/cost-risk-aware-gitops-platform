// Package queue is the shared work queue backing the demo workload.
//
// It is deliberately a *shared* store rather than an in-process channel. M5's scaling
// claim is "demand drives replica count, and replicas drain demand"; that claim is only
// falsifiable if every replica draws from the same backlog. An in-process queue would
// scale on a number each replica computes about itself, which can never be wrong.
package queue

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/redis/go-redis/v9"
	"github.com/redis/go-redis/v9/maintnotifications"
)

// Key is the Redis list holding pending jobs. KEDA's Redis scaler is pointed at this
// same key, so the autoscaler and the application agree on what "demand" means by
// construction rather than by convention.
const Key = "jobs:pending"

// Job is the unit of work. EnqueuedAt travels with the job so the worker can report
// queueing delay — the consumer is the only party that knows when the wait ended.
type Job struct {
	ID         string    `json:"id"`
	DurationMS int       `json:"duration_ms"`
	EnqueuedAt time.Time `json:"enqueued_at"`
}

// ErrEmpty is returned by Dequeue when the blocking pop timed out with no work.
// This is an ordinary idle condition, not a failure, and callers must not treat it
// as one — a worker that logged an error every idle second would bury real faults.
var ErrEmpty = errors.New("queue: empty")

type Queue struct {
	client *redis.Client
}

func New(addr, password string) *Queue {
	return &Queue{client: redis.NewClient(&redis.Options{
		Addr:     addr,
		Password: password,
		// Bounded so a wedged backend surfaces as a failing readiness probe rather
		// than as requests that hang until the client gives up.
		DialTimeout:  3 * time.Second,
		ReadTimeout:  5 * time.Second,
		WriteTimeout: 3 * time.Second,

		// Maintenance notifications are a Redis Cloud feature. The client's default
		// "auto" mode probes for it on every connection, and self-hosted Redis rejects
		// the probe — harmless, but it logs an error-shaped line on each reconnect.
		// Disabling it keeps the logs free of failures that are not failures, which
		// matters when the logs are shown during a demonstration.
		MaintNotificationsConfig: &maintnotifications.Config{
			Mode: maintnotifications.ModeDisabled,
		},
	})}
}

func (q *Queue) Close() error { return q.client.Close() }

// Ping backs the readiness probe. The API cannot accept work it has nowhere to put,
// so readiness must depend on the queue being reachable.
func (q *Queue) Ping(ctx context.Context) error {
	return q.client.Ping(ctx).Err()
}

func (q *Queue) Enqueue(ctx context.Context, job Job) error {
	payload, err := json.Marshal(job)
	if err != nil {
		return fmt.Errorf("marshal job: %w", err)
	}
	// LPUSH with the worker's BRPOP gives FIFO ordering: jobs enter at the head and
	// leave from the tail, so a burst does not starve the work that preceded it.
	if err := q.client.LPush(ctx, Key, payload).Err(); err != nil {
		return fmt.Errorf("enqueue: %w", err)
	}
	return nil
}

// Dequeue blocks for up to timeout waiting for a job. Blocking rather than polling
// keeps an idle worker's CPU — and therefore its attributed cost in M4 — near zero,
// which is what makes a scale-down visibly cheaper on the dashboard.
func (q *Queue) Dequeue(ctx context.Context, timeout time.Duration) (Job, error) {
	res, err := q.client.BRPop(ctx, timeout, Key).Result()
	if errors.Is(err, redis.Nil) {
		return Job{}, ErrEmpty
	}
	if err != nil {
		return Job{}, fmt.Errorf("dequeue: %w", err)
	}
	// BRPOP returns [key, value].
	if len(res) != 2 {
		return Job{}, fmt.Errorf("dequeue: unexpected reply length %d", len(res))
	}
	var job Job
	if err := json.Unmarshal([]byte(res[1]), &job); err != nil {
		// A malformed entry is dropped rather than retried: re-queueing it would
		// block the queue head forever behind a job that can never succeed.
		return Job{}, fmt.Errorf("decode job: %w", err)
	}
	return job, nil
}

// Depth reports the shared backlog. This is the number M5 scales on and M7 charts.
func (q *Queue) Depth(ctx context.Context) (int64, error) {
	n, err := q.client.LLen(ctx, Key).Result()
	if err != nil {
		return 0, fmt.Errorf("depth: %w", err)
	}
	return n, nil
}

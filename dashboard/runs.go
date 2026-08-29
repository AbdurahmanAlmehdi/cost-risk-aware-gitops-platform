package main

// The demonstrations themselves.
//
// Both follow the same contract: stream what is happening as it happens, assert the claim
// at the end, and return an error if the claim did not hold. The browser renders whatever
// arrives, so a failure is as visible as a success, which is the point. A demonstration
// that can only succeed is a slideshow.

import (
	"context"
	"fmt"
	"time"
)

// -----------------------------------------------------------------------------
// Drift. Git governs the cluster
// -----------------------------------------------------------------------------

func (s *server) runDrift(ctx context.Context, emit emitFunc) error {
	const phase = "drift"

	info(emit, phase, "Reading what Git declares for %s/%s…", demoNS, apiDeployment)
	dep, err := s.kube.deployment(ctx, demoNS, apiDeployment)
	if err != nil {
		return fmt.Errorf("could not read the deployment: %w", err)
	}
	declared, index, ok := dep.memoryLimit(apiContainer)
	if !ok || declared == "" {
		return fmt.Errorf("no memory limit on container %q, is the workload deployed?", apiContainer)
	}
	emit(event{Phase: phase, Level: "info",
		Message: fmt.Sprintf("Git declares a memory limit of %s.", declared),
		Metrics: map[string]any{"declared": declared}})

	// Without this the test could "pass" simply because the declared value already
	// equalled the drift value and nothing ever changed.
	if declared == driftedMemory {
		return fmt.Errorf("the declared value is already %s, so the test would prove nothing", driftedMemory)
	}

	info(emit, phase, "Editing the live cluster directly, setting the limit to %s, the way an engineer would at 3am.", driftedMemory)
	if err := s.kube.patchMemoryLimit(ctx, demoNS, apiDeployment, index, driftedMemory); err != nil {
		return fmt.Errorf("could not apply the drift: %w", err)
	}

	// Confirm the drift actually landed, or there is nothing for the platform to detect.
	dep, err = s.kube.deployment(ctx, demoNS, apiDeployment)
	if err != nil {
		return fmt.Errorf("could not re-read the deployment: %w", err)
	}
	if now, _, _ := dep.memoryLimit(apiContainer); now != driftedMemory {
		return fmt.Errorf("the drift did not apply (limit reads %q), so there is nothing to detect", now)
	}
	emit(event{Phase: phase, Level: "info",
		Message: "Drift applied, the cluster now disagrees with Git. Nobody is going to fix this by hand.",
		Metrics: map[string]any{"current": driftedMemory}})

	info(emit, phase, "Waiting for the platform to revert it, unattended (up to %ds)…", int(driftTimeout.Seconds()))
	started := time.Now()
	ticker := time.NewTicker(2 * time.Second)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-ticker.C:
		}
		dep, err := s.kube.deployment(ctx, demoNS, apiDeployment)
		if err != nil {
			return fmt.Errorf("lost sight of the deployment: %w", err)
		}
		current, _, _ := dep.memoryLimit(apiContainer)
		elapsed := int(time.Since(started).Seconds())
		if current == declared {
			emit(event{Phase: phase, Level: "pass",
				Message: fmt.Sprintf("Reverted after about %ds, with no human involved. Live state was corrected to match Git.", elapsed),
				Metrics: map[string]any{"elapsed_seconds": elapsed, "restored": declared}})
			return nil
		}
		emit(event{Phase: phase, Level: "info",
			Message: fmt.Sprintf("t+%ds, still %s", elapsed, current),
			Metrics: map[string]any{"elapsed_seconds": elapsed, "current": current}})
		if time.Since(started) > driftTimeout {
			return fmt.Errorf("the manual change survived %ds, self-heal is not reverting drift", int(driftTimeout.Seconds()))
		}
	}
}

// -----------------------------------------------------------------------------
// Load, demand drives replicas, and the backlog actually clears
// -----------------------------------------------------------------------------

func (s *server) runLoad(ctx context.Context, emit emitFunc) error {
	const phase = "load"

	minR, maxR, err := s.kube.bounds(ctx, demoNS, workerDeploy)
	if err != nil {
		return err
	}
	info(emit, phase, "Worker tier is authorised to run between %d and %d replicas.", minR, maxR)

	// A bounds violation at any point is a failure, so replicas are checked on every poll
	// rather than only at the end, a transient overshoot is exactly the bug this catches.
	assertBounds := func(r int) error {
		if r > maxR {
			return fmt.Errorf("replicas reached %d, above the authorised maximum of %d", r, maxR)
		}
		if r < minR {
			return fmt.Errorf("replicas fell to %d, below the authorised minimum of %d", r, minR)
		}
		return nil
	}

	dep, err := s.kube.deployment(ctx, demoNS, workerDeploy)
	if err != nil {
		return fmt.Errorf("could not read the worker deployment: %w", err)
	}
	startReplicas := dep.Status.Replicas
	emit(event{Phase: phase, Level: "info",
		Message: fmt.Sprintf("Starting at %d replica(s).", startReplicas),
		Metrics: map[string]any{"replicas": startReplicas, "min": minR, "max": maxR}})

	info(emit, phase, "Putting %d jobs on the queue. Nothing is being reconfigured, the system reacts on its own.", loadJobs)
	if err := s.api.enqueue(ctx, loadJobs, loadJobMS); err != nil {
		return fmt.Errorf("could not reach the demo API to enqueue work: %w", err)
	}

	poll := func(deadline time.Duration, stage string, done func(replicas int, depth int64) bool) (int, error) {
		started := time.Now()
		peak := startReplicas
		ticker := time.NewTicker(pollInterval)
		defer ticker.Stop()
		for {
			dep, err := s.kube.deployment(ctx, demoNS, workerDeploy)
			if err != nil {
				return peak, fmt.Errorf("lost sight of the worker tier: %w", err)
			}
			r := dep.Status.Replicas
			if err := assertBounds(r); err != nil {
				return peak, err
			}
			if r > peak {
				peak = r
			}
			depth, depthErr := s.api.queueDepth(ctx)
			depthValue := any(depth)
			if depthErr != nil {
				depthValue = nil
			}
			emit(event{Phase: phase, Level: "info",
				Message: fmt.Sprintf("t+%ds - %d replica(s), %s job(s) queued",
					int(time.Since(started).Seconds()), r, formatDepth(depth, depthErr)),
				Metrics: map[string]any{"replicas": r, "queue_depth": depthValue, "stage": stage}})
			if depthErr == nil && done(r, depth) {
				return peak, nil
			}
			if time.Since(started) > deadline {
				return peak, fmt.Errorf("%s did not happen within %ds", stage, int(deadline.Seconds()))
			}
			select {
			case <-ctx.Done():
				return peak, ctx.Err()
			case <-ticker.C:
			}
		}
	}

	info(emit, phase, "Watching for the platform to respond…")
	peak, err := poll(scaleTimeout, "scale-up", func(r int, _ int64) bool { return r > startReplicas })
	if err != nil {
		return fmt.Errorf("demand rose but replicas did not: %w", err)
	}
	emit(event{Phase: phase, Level: "info",
		Message: fmt.Sprintf("Scaled up to %d replicas.", peak),
		Metrics: map[string]any{"peak_replicas": peak}})

	// The part that matters most. Replica count alone proves only that the autoscaler
	// acted; a workload can scale up and still fall behind. The queue draining is what
	// shows the action had the intended effect.
	info(emit, phase, "Waiting for the backlog to drain. This is what proves the extra replicas did the work.")
	drainPeak, err := poll(drainTimeout, "drain", func(_ int, depth int64) bool { return depth == 0 })
	if err != nil {
		return fmt.Errorf("the backlog did not clear: %w", err)
	}
	if drainPeak > peak {
		peak = drainPeak
	}

	emit(event{Phase: phase, Level: "pass",
		Message: fmt.Sprintf("Demand rose, replicas followed to %d (never leaving %d–%d), and the backlog cleared. Scale-down is deliberately slow, so replicas settle back to %d over the next few minutes.",
			peak, minR, maxR, minR),
		Metrics: map[string]any{"peak_replicas": peak, "min": minR, "max": maxR}})
	return nil
}

func formatDepth(depth int64, err error) string {
	if err != nil {
		return "an unknown number of"
	}
	return fmt.Sprintf("%d", depth)
}

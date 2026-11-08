package scheduler

import (
	"context"
	"log/slog"
	"time"
)

type PRConflictWakeupEnsurer interface {
	EnsurePRConflictWakeups(context.Context, int32) (int, error)
}

// PRConflictWakeupJob gives agent-assigned issues with an open linked pull
// request their "wake on merge conflicts" rule; see
// service.IssueWakeupService.EnsurePRConflictWakeups.
func PRConflictWakeupJob(ensurer PRConflictWakeupEnsurer) JobSpec {
	return JobSpec{
		Name: "pr_conflict_wakeup_sweep", Cadence: 2 * time.Minute, CatchUpMode: CatchUpLatestOnly, CatchUpWindow: time.Hour,
		RunTimeout: 90 * time.Second, StaleTimeout: 3 * time.Minute, HeartbeatInterval: 10 * time.Second,
		AllowStaleReentry: true, MaxAttempts: 1, Scopes: StaticScopes(ScopeGlobal),
		Handler: func(ctx context.Context, _ HandlerInput) (HandlerResult, error) {
			created, err := ensurer.EnsurePRConflictWakeups(ctx, 100)
			if created > 0 {
				slog.Info("pr conflict wakeup sweep", "created", created)
			}
			return HandlerResult{}, err
		},
	}
}

package runtime

import (
	"context"
	"log/slog"
	"time"

	"github.com/nextlevelbuilder/goclaw/internal/store"
)

// RunRecoveryScan looks for runs left in queued/running state with no
// recent heartbeat (their goclaw instance crashed mid-flight) and marks
// them as `error` so they don't show "running forever" in the UI. The
// SPA's bubble offers a "Retry failed cells" action that creates a new
// run scoped to the unfinished cells of the original.
//
// We intentionally do NOT auto-resume here — auto-resume on a different
// instance risks duplicating Sheet writes if the original instance's
// pending batch is still being delivered. Manual user-driven retry is
// safer and matches the "no surprise side-effects after crash" rule.
//
// Designed to be called periodically (every ~60s) by a goclaw worker
// goroutine started at process boot.
func RunRecoveryScan(ctx context.Context, st store.SheetWorkflowStore, staleAfter time.Duration) (markedErrored int, err error) {
	runs, err := st.ListRecoverableRuns(ctx, staleAfter)
	if err != nil {
		return 0, err
	}
	msg := "instance crashed before run completed; retry from bubble to enrich remaining cells"
	for _, r := range runs {
		if err := st.FinishRun(ctx, r.ID, "error", &msg); err != nil {
			slog.Warn("recovery: finish stale run", "run", r.ID, "err", err)
			continue
		}
		markedErrored++
		slog.Info("recovery: marked stale run as error", "run", r.ID, "workflow", r.WorkflowID)
	}
	return markedErrored, nil
}

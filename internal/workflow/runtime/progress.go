package runtime

import "sync/atomic"

// progressTracker is a thread-safe counter set for an in-flight run.
// Accessed concurrently by every worker goroutine in the wave; flushed
// to DB + WS bus between waves and on completion.
type progressTracker struct {
	totalRows int
	totalCols int

	completed atomic.Int64
	errored   atomic.Int64
	tokensIn  atomic.Int64
	tokensOut atomic.Int64
}

func newProgressTracker(rows, cols int) *progressTracker {
	return &progressTracker{totalRows: rows, totalCols: cols}
}

func (p *progressTracker) totalCells() int { return p.totalRows * p.totalCols }

func (p *progressTracker) cellDone(tokIn, tokOut int) {
	p.completed.Add(1)
	p.tokensIn.Add(int64(tokIn))
	p.tokensOut.Add(int64(tokOut))
}

func (p *progressTracker) cellError() {
	p.errored.Add(1)
}

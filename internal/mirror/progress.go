package mirror

import (
	"io"
	"sync/atomic"
	"time"
)

// progressInterval is how often a transfer in flight reports itself.
//
// Fast enough to look live, slow enough that a 4 GiB file does not emit
// thousands of events on its way through.
const progressInterval = 250 * time.Millisecond

// progressReader reports how much of a transfer has moved, as it moves.
//
// Transfers were previously only visible once finished, so a large file
// looked like a stall: nothing on screen for minutes, then one line. This
// wraps the single io.Copy that every transfer funnels through.
type progressReader struct {
	r     io.Reader
	total int64

	done     atomic.Int64
	lastEmit time.Time
	report   func(done, total int64)
}

func newProgressReader(r io.Reader, total int64, report func(done, total int64)) *progressReader {
	return &progressReader{r: r, total: total, report: report, lastEmit: time.Now()}
}

func (p *progressReader) Read(b []byte) (int, error) {
	n, err := p.r.Read(b)
	if n > 0 {
		done := p.done.Add(int64(n))
		if p.report != nil && time.Since(p.lastEmit) >= progressInterval {
			p.lastEmit = time.Now()
			p.report(done, p.total)
		}
	}
	return n, err
}

// Done returns the bytes transferred so far.
func (p *progressReader) Done() int64 { return p.done.Load() }

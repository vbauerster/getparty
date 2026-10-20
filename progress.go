package getparty

import (
	"context"
	"fmt"
	"io"
	"sync/atomic"
	"time"

	"github.com/vbauerster/mpb/v8"
	"github.com/vbauerster/mpb/v8/decor"
)

type progress struct {
	*mpb.Progress
	nopBar  *mpb.Bar
	totalCh chan int64
	out     io.Writer
}

func newProgress(ctx context.Context, out, err io.Writer, plen int) *progress {
	p := mpb.NewWithContext(ctx,
		mpb.WithOutput(out),
		mpb.WithDebugOutput(err),
		mpb.WithRefreshRate(refreshRate*time.Millisecond),
		mpb.WithWidth(64),
		mpb.WithQueueLen(plen+3), // +3 to account nopBar totalBar and mergeBar
	)
	return &progress{
		Progress: p,
		nopBar:   p.New(0, nil),
		totalCh:  make(chan int64, min(plen, 12)),
		out:      out,
	}
}

func (p *progress) Wait() {
	p.nopBar.EnableTriggerComplete()
	p.Progress.Wait()
	_, _ = fmt.Fprintln(p.out)
}

// incrTotal invariant: addTotalBar must be called before calling this one
// otherwise incrTotal will block after p.totalCh is full.
func (p *progress) incrTotal(n int64) {
	p.totalCh <- n
}

func (p *progress) addTotalBar(
	doneCount *atomic.Uint32,
	partCount int,
	contentLength int64,
	start time.Time,
) (*mpb.Bar, error) {
	return p.Add(contentLength, barBuilder.Build(),
		mpb.BarFillerTrim(),
		mpb.BarPriority(partCount+1),
		mpb.PrependDecorators(
			decor.Any(func(_ decor.Statistics) string {
				return fmt.Sprintf("Total(%d/%d)", doneCount.Load(), partCount)
			}, decor.WCSyncWidthR),
			decor.OnComplete(decor.NewPercentage("%.2f", decor.WCSyncSpace), "100%"),
		),
		mpb.AppendDecorators(
			decor.OnCompleteOrOnAbort(decor.NewAverageETA(
				decor.ET_STYLE_MMSS,
				start,
				nil,
				decor.WCSyncWidth,
			), "avrg:"),
			decor.NewAverageSpeed(decor.SizeB1024(0), "%.1f", start, decor.WCSyncSpace),
		),
	)
}

func (p *progress) addMergeBar(partCount int) (*mpb.Bar, error) {
	return p.Add(int64(partCount), barBuilder.Build(),
		mpb.BarFillerTrim(),
		mpb.BarPriority(partCount+2),
		mpb.PrependDecorators(
			decor.CountersNoUnit("Merge(%d/%d)", decor.WCSyncWidthR),
			decor.NewPercentage("%d", decor.WCSyncSpace),
		),
		mpb.AppendDecorators(
			decor.Name("", decor.WCSyncWidth),
			decor.Name("", decor.WCSyncWidth),
		),
	)
}

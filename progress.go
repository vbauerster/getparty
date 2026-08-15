package getparty

import (
	"context"
	"errors"
	"fmt"
	"io"
	"sync"
	"sync/atomic"
	"time"

	"github.com/vbauerster/mpb/v8"
	"github.com/vbauerster/mpb/v8/decor"
)

type progress struct {
	*mpb.Progress
	nopBar   *mpb.Bar
	totalBar *mpb.Bar
	totalWg  *sync.WaitGroup
	totalUpd chan int
	out      io.Writer
}

func newProgress(ctx context.Context, out, err io.Writer, plen int) *progress {
	totalWg := new(sync.WaitGroup)
	p := mpb.NewWithContext(ctx,
		mpb.WithOutput(out),
		mpb.WithDebugOutput(err),
		mpb.WithRefreshRate(refreshRate*time.Millisecond),
		mpb.WithWidth(64),
		mpb.WithWaitGroup(totalWg),
		mpb.WithQueueLen(plen+3), // +3 to account nopBar totalBar and mergeBar
	)
	return &progress{
		Progress: p,
		nopBar:   p.New(0, nil),
		totalWg:  totalWg,
		totalUpd: make(chan int, min(plen, 12)),
		out:      out,
	}
}

func (p *progress) Wait() {
	if p.totalBar != nil {
		p.totalBar.Abort(false)
	}
	p.nopBar.EnableTriggerComplete()
	p.Progress.Wait()
	_, _ = fmt.Fprintln(p.out)
}

// incrTotal invariant: runTotalBar must be called once before calling
// this one otherwise incrTotal will block after totalUpd chan is full.
func (p *progress) incrTotal(n int) {
	p.totalUpd <- n
}

func (p *progress) runTotalBar(start time.Time, contentLength int64, partCount int, doneCount *atomic.Uint32) {
	if p.totalBar != nil {
		panic(errors.New("runTotalBar must be called once"))
	}

	bar := p.New(contentLength, barBuilder,
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

	for range max(cap(p.totalUpd)/3, 1) {
		p.totalWg.Go(func() {
			for n := range p.totalUpd {
				bar.IncrBy(n)
			}
		})
	}

	p.totalBar = bar
}

func (p *progress) setCurrent(current int64) {
	if p.totalBar == nil {
		panic(errors.New("setCurrent is called before runTotalBar"))
	}
	if current <= 0 {
		return
	}
	p.totalBar.SetCurrent(current)
	p.totalBar.SetRefillCurrent()
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

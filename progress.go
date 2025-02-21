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
	topBar  *mpb.Bar
	total   chan int
	current int64
	out     io.Writer
	err     io.Writer
}

func (p *progress) Wait() {
	if p.total != nil {
		close(p.total)
	}
	p.topBar.EnableTriggerComplete()
	p.Progress.Wait()
	fmt.Fprintln(p.out)
}

func (p *progress) runTotalBar(contentLength int64, done *uint32, total int, start time.Time) {
	bar := p.MustAdd(contentLength, distinctBarRefiller(baseBarStyle()).Build(),
		mpb.BarFillerTrim(),
		mpb.BarPriority(total+1),
		mpb.PrependDecorators(
			decor.Any(func(_ decor.Statistics) string {
				return fmt.Sprintf("Total(%d/%d)", atomic.LoadUint32(done), total)
			}, decor.WCSyncWidthR),
			decor.OnComplete(decor.NewPercentage("%.2f", decor.WCSyncSpace), "100%"),
		),
		mpb.AppendDecorators(
			decor.OnCompleteOrOnAbort(decor.NewAverageETA(
				decor.ET_STYLE_MMSS,
				start,
				nil,
				decor.WCSyncWidth), ":"),
			decor.NewAverageSpeed(decor.SizeB1024(0), "%.1f", start, decor.WCSyncSpace),
			decor.Name("", decor.WCSyncSpace),
			decor.Name("", decor.WCSyncSpace),
		),
	)
	go func() {
		for n := range p.total {
			bar.IncrBy(n)
		}
		bar.Abort(false)
	}()
	if p.current != 0 {
		bar.SetCurrent(p.current)
		bar.SetRefill(p.current)
	}
}

func newProgress(ctx context.Context, session *Session, out, err io.Writer) *progress {
	var total chan int
	qlen := 1
	for _, p := range session.Parts {
		if !p.isDone() {
			qlen++
		}
	}
	if !session.Single {
		total = make(chan int, qlen)
		qlen += 2 // account for total and concat bars
	}
	p := mpb.NewWithContext(ctx,
		mpb.WithOutput(out),
		mpb.WithDebugOutput(err),
		mpb.WithRefreshRate(refreshRate*time.Millisecond),
		mpb.WithWidth(64),
		mpb.WithQueueLen(qlen),
	)
	return &progress{
		Progress: p,
		topBar:   p.MustAdd(0, nil),
		total:    total,
		current:  session.totalWritten(),
		out:      out,
		err:      err,
	}
}

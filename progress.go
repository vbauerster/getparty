package getparty

import (
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
}

func (p *progress) Wait() {
	if p.total != nil {
		close(p.total)
	}
	p.topBar.EnableTriggerComplete()
	p.Progress.Wait()
	_, _ = fmt.Fprintln(p.out)
}

func (p *progress) incrTotal(n int) {
	p.total <- n
}

func (p *progress) runTotalBar(start time.Time, contentLength int64, partCount int, doneCount *atomic.Uint32) {
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
	go func() {
		defer bar.Abort(false)
		for n := range p.total {
			bar.IncrBy(n)
		}
	}()
	if p.current != 0 {
		bar.SetCurrent(p.current)
		bar.SetRefillCurrent()
	}
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

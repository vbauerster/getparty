package getparty

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"os"
	"strings"
	"sync/atomic"
	"time"

	"github.com/vbauerster/mpb/v8"
	"github.com/vbauerster/mpb/v8/decor"
)

type progress struct {
	*mpb.Progress
	topBar  *mpb.Bar
	total   chan int
	written int64
	out     io.Writer
}

func (p *progress) Wait() {
	if p.total != nil {
		close(p.total)
	}
	p.topBar.EnableTriggerComplete()
	p.Progress.Wait()
	fmt.Fprintln(p.out)
}

// Session represents download session state
type Session struct {
	restored      bool
	location      string
	URL           string
	OutputName    string
	ContentMD5    string
	AcceptRanges  string
	ContentType   string
	StatusCode    int
	ContentLength int64
	Redirected    bool
	Single        bool
	Elapsed       time.Duration
	HeaderMap     map[string]string
	Parts         []*Part
}

func (s *Session) calcParts(parts uint) error {
	if parts == 0 {
		return ErrZeroParts
	}
	if !s.isResumable() {
		parts = 1
	}

	fragment := s.ContentLength / int64(parts)
	if parts != 1 && fragment < 64 {
		return ErrTooFragmented
	}

	s.Parts = make([]*Part, parts)
	s.Parts[0] = new(Part)

	var stop int64
	start := s.ContentLength
	for i := parts - 1; i > 0; i-- {
		stop = start - 1
		start = stop - fragment
		s.Parts[i] = &Part{
			Start: start,
			Stop:  stop,
		}
	}

	// if session isn't resumable stop is always negative
	s.Parts[0].Stop = start - 1

	s.Single = parts == 1
	return nil
}

func (s *Session) loadState(name string) error {
	f, err := os.Open(name)
	if err != nil {
		return withStack(err)
	}
	err = firstErr(json.NewDecoder(f).Decode(s), f.Close())
	return withStack(err)
}

func (s *Session) dumpState(name string) error {
	f, err := os.Create(name)
	if err != nil {
		return withStack(err)
	}
	err = firstErr(json.NewEncoder(f).Encode(s), f.Close())
	return withStack(err)
}

func (s Session) isResumable() bool {
	return strings.EqualFold(s.AcceptRanges, "bytes") && s.ContentLength >= 0
}

func (s Session) totalWritten() int64 {
	var total int64
	for _, p := range s.Parts {
		total += p.Written
	}
	return total
}

func (s Session) summary(loggers [lEVELS]*log.Logger) {
	format := fmt.Sprintf("Length: %%s [%s]", s.ContentType)
	switch {
	case s.isResumable():
		summary := fmt.Sprintf("%d (%.1f)", s.ContentLength, decor.SizeB1024(s.ContentLength))
		loggers[INFO].Printf(format, summary)
		if tw := s.totalWritten(); tw != 0 {
			remaining := s.ContentLength - tw
			loggers[INFO].Printf("Remaining: %d (%.1f)", remaining, decor.SizeB1024(remaining))
		}
	case s.ContentLength < 0:
		loggers[INFO].Printf(format, "unknown")
		fallthrough
	default:
		message := "Session is not resumable"
		loggers[WARN].Println(message)
		loggers[DEBUG].Println(message)
	}
}

func (s Session) isOutputFileExist() (bool, error) {
	stat, err := os.Stat(s.OutputName)
	if errors.Is(err, os.ErrNotExist) {
		return false, nil
	}
	if err == nil && stat.IsDir() {
		return true, fmt.Errorf("%q is a directory", s.OutputName)
	}
	return true, err
}

func (s Session) checkContentSums(other Session) error {
	if s.ContentLength != other.ContentLength {
		return fmt.Errorf("ContentLength mismatch: expected %d got %d", s.ContentLength, other.ContentLength)
	}
	if s.ContentMD5 != other.ContentMD5 {
		return fmt.Errorf("%s mismatch: expected %q got %q", hContentMD5, s.ContentMD5, other.ContentMD5)
	}
	return nil
}

func (s Session) newProgress(ctx context.Context, out, err io.Writer) *progress {
	var total chan int
	qlen := 1
	for _, p := range s.Parts {
		if !p.isDone() {
			qlen++
		}
	}
	if !s.Single {
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
		written:  s.totalWritten(),
		out:      out,
	}
}

func (s Session) runTotalBar(progress *progress, doneCount *uint32, start time.Time) {
	start = start.Add(-s.Elapsed)
	bar := progress.MustAdd(s.ContentLength, distinctBarRefiller(baseBarStyle()).Build(),
		mpb.BarFillerTrim(),
		mpb.BarPriority(len(s.Parts)+1),
		mpb.PrependDecorators(
			decor.Any(func(_ decor.Statistics) string {
				return fmt.Sprintf("Total(%d/%d)", atomic.LoadUint32(doneCount), len(s.Parts))
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
		for n := range progress.total {
			bar.IncrBy(n)
		}
		bar.Abort(false)
	}()
	if progress.written != 0 {
		bar.SetCurrent(progress.written)
		bar.SetRefill(progress.written)
	}
}

func (s Session) concatenateParts(progress *progress) error {
	if tw := s.totalWritten(); tw != s.ContentLength {
		return withStack(fmt.Errorf("ContentLength mismatch: written=%d ContentLength=%d", tw, s.ContentLength))
	}

	bar, err := progress.Add(int64(len(s.Parts)-1), baseBarStyle().Build(),
		mpb.BarFillerTrim(),
		mpb.BarPriority(len(s.Parts)+2),
		mpb.PrependDecorators(
			decor.Name("Concatenating", decor.WCSyncWidthR),
			decor.NewPercentage("%d", decor.WCSyncSpace),
		),
		mpb.AppendDecorators(
			decor.OnComplete(decor.AverageETA(decor.ET_STYLE_MMSS, decor.WCSyncWidth), ":"),
			decor.Name("", decor.WCSyncSpace),
			decor.Name("", decor.WCSyncSpace),
			decor.Name("", decor.WCSyncSpace),
		),
	)
	if err != nil {
		return withStack(err)
	}

	p := s.Parts[0]
	err = p.checkSize()
	if err != nil {
		return errors.Join(p.file.Close(), err)
	}

	dst := p.file
	if dst == nil {
		return withStack(fmt.Errorf("expected non nil file on %s", p.name))
	}

	for _, p := range s.Parts[1:] {
		err := p.writeTo(dst)
		if err != nil {
			bar.Abort(false)
			return errors.Join(dst.Close(), err)
		}
		bar.Increment()
	}

	err = firstErr(dst.Sync(), dst.Close())
	if err == nil {
		err = os.Rename(dst.Name(), s.OutputName)
		p.logger.Printf("%q renamed to %q with: %v", dst.Name(), s.OutputName, err)
	}
	return withStack(err)
}

package getparty

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"os"
	"strings"
	"sync/atomic"
	"time"

	"github.com/pkg/errors"
	"github.com/vbauerster/mpb/v8"
	"github.com/vbauerster/mpb/v8/decor"
)

// Session represents download session state
type Session struct {
	location      string
	URL           string
	OutputName    string
	ContentMD5    string
	AcceptRanges  string
	ContentType   string
	StatusCode    int
	ContentLength int64
	Redirected    bool
	Elapsed       time.Duration
	HeaderMap     map[string]string
	Parts         []*Part
}

func (s *Session) calcParts(parts uint) error {
	if !s.isResumable() {
		parts = 1
	}

	fragment := s.ContentLength / int64(parts)
	if parts != 1 && fragment < 64 {
		return errors.New("Too fragmented")
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

	s.Parts[0].Stop = start - 1

	return nil
}

func (s *Session) loadState(name string) error {
	f, err := os.Open(name)
	if err != nil {
		return err
	}
	return firstErr(json.NewDecoder(f).Decode(s), f.Close())
}

func (s *Session) dumpState(name string) error {
	f, err := os.Create(name)
	if err != nil {
		return err
	}
	return firstErr(json.NewEncoder(f).Encode(s), f.Close())
}

func (s Session) isResumable() bool {
	return strings.EqualFold(s.AcceptRanges, "bytes") && s.ContentLength > 0
}

func (s Session) totalWritten() int64 {
	var total int64
	for _, p := range s.Parts {
		total += p.Written
	}
	return total
}

func (s Session) summary(loggers [LEVELS]*log.Logger) {
	format := fmt.Sprintf("Length: %%s [%s]", s.ContentType)
	if s.ContentLength <= 0 {
		loggers[INFO].Printf(format, "unknown")
	} else {
		summary := fmt.Sprintf("%d (%.1f)", s.ContentLength, decor.SizeB1024(s.ContentLength))
		loggers[INFO].Printf(format, summary)
		if total := s.totalWritten(); total != 0 {
			remaining := s.ContentLength - total
			loggers[INFO].Printf("Remaining: %d (%.1f)", remaining, decor.SizeB1024(remaining))
		}
	}
	if len(s.Parts) != 0 {
		loggers[INFO].Printf("Saving to: %q", s.OutputName)
	}
	if !s.isResumable() {
		loggers[WARN].Println("Session is not resumable")
	}
}

func (s Session) isOutputFileExist() (bool, error) {
	stat, err := os.Stat(s.OutputName)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return false, nil
		}
		return false, err
	}
	if stat.IsDir() {
		return true, errors.Wrapf(os.ErrInvalid, "%q is a directory", s.OutputName)
	}
	return true, nil
}

func (s Session) checkContentSums(other Session) error {
	if s.ContentLength != other.ContentLength {
		return errors.Errorf("ContentLength mismatch: expected %d got %d", s.ContentLength, other.ContentLength)
	}
	if s.ContentMD5 != other.ContentMD5 {
		return errors.Errorf("%s mismatch: expected %q got %q", hContentMD5, s.ContentMD5, other.ContentMD5)
	}
	return nil
}

func (s Session) checkSizeOfEachPart() error {
	for i, p := range s.Parts {
		if p.Written == 0 {
			continue
		}
		p.order = i + 1
		p.sessionOutputName = s.OutputName
		stat, err := os.Stat(p.outputName())
		if err != nil {
			return err
		}
		err = p.checkSize(stat)
		if err != nil {
			return err
		}
	}
	return nil
}

func (s Session) isHttpStatusOK() bool {
	for _, p := range s.Parts {
		if p.httpStatusOK {
			return true
		}
	}
	return false
}

func (s Session) concatenateParts(progress *mpb.Progress) (err error) {
	if len(s.Parts) <= 1 {
		return nil
	}
	if s.isHttpStatusOK() {
		return errors.New("Cannot concatenate session with http status 200")
	}
	if tw := s.totalWritten(); tw != s.ContentLength {
		return errors.Errorf("Written count mismatch: ContentLength %d, written %d", s.ContentLength, tw)
	}

	bar, err := progress.Add(int64(len(s.Parts)-1), baseBarStyle().Build(),
		mpb.BarFillerTrim(),
		mpb.BarPriority(len(s.Parts)+1),
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
		return err
	}

	dst, err := os.OpenFile(s.OutputName, os.O_WRONLY|os.O_CREATE|os.O_TRUNC, umask)
	if err != nil {
		return err
	}
	defer func() {
		err = firstErr(err, dst.Close())
		bar.Abort(false) // if bar is completed bar.Abort is nop
	}()

	for _, p := range s.Parts {
		err = p.writeTo(dst)
		if err != nil {
			return err
		}
		bar.Increment()
	}

	return nil
}

func (s Session) runTotalBar(
	ctx context.Context,
	doneCount *uint32,
	progress *mpb.Progress,
	quiet bool,
) (func(int), func(bool), error) {
	if len(s.Parts) <= 1 || quiet {
		return func(int) {}, func(bool) {}, nil
	}
	bar, err := progress.Add(s.ContentLength, distinctBarRefiller(baseBarStyle()).Build(),
		mpb.BarFillerTrim(),
		mpb.BarExtender(mpb.BarFillerFunc(
			func(w io.Writer, _ decor.Statistics) error {
				_, err := fmt.Fprintln(w)
				return err
			}), true),
		mpb.PrependDecorators(
			decor.Any(func(_ decor.Statistics) string {
				return fmt.Sprintf("Total(%d/%d)", atomic.LoadUint32(doneCount), len(s.Parts))
			}, decor.WCSyncWidthR),
			decor.OnComplete(decor.NewPercentage("%.2f", decor.WCSyncSpace), "100%"),
		),
		mpb.AppendDecorators(
			decor.OnCompleteOrOnAbort(decor.AverageETA(decor.ET_STYLE_MMSS, decor.WCSyncWidth), ":"),
			decor.AverageSpeed(decor.SizeB1024(0), "%.1f", decor.WCSyncSpace),
			decor.Name("", decor.WCSyncSpace),
			decor.Name("", decor.WCSyncSpace),
		),
	)
	if err != nil {
		return nil, nil, err
	}
	if written := s.totalWritten(); written != 0 {
		bar.SetCurrent(written)
		bar.SetRefill(written)
		bar.DecoratorAverageAdjust(time.Now().Add(-s.Elapsed))
	}
	ch := make(chan int, len(s.Parts)-int(atomic.LoadUint32(doneCount)))
	ctx, cancel := context.WithCancel(ctx)
	dropCtx, dropCancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() {
		defer close(done)
		for {
			select {
			case n := <-ch:
				bar.IncrBy(n)
			case <-dropCtx.Done():
				cancel()
				bar.Abort(true)
				return
			case <-ctx.Done():
				dropCancel()
				bar.Abort(false)
				return
			}
		}
	}()
	return func(n int) {
			select {
			case ch <- n:
			case <-done:
			}
		}, func(drop bool) {
			if drop {
				dropCancel()
			} else {
				cancel()
			}
		}, nil
}

package getparty

import (
	"bufio"
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
	location       string
	URL            string
	OutputFileName string
	ContentMD5     string
	AcceptRanges   string
	ContentType    string
	StatusCode     int
	ContentLength  int64
	Redirected     bool
	Elapsed        time.Duration
	HeaderMap      map[string]string
	Parts          []*Part
}

func (s Session) isResumable() bool {
	return strings.EqualFold(s.AcceptRanges, "bytes") && s.ContentLength > 0
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
	s.Parts[0] = &Part{
		FileName: s.OutputFileName,
	}

	var stop int64
	start := s.ContentLength
	for i := parts - 1; i > 0; i-- {
		stop = start - 1
		start = stop - fragment
		s.Parts[i] = &Part{
			FileName: fmt.Sprintf("%s.%02d", s.OutputFileName, i),
			Start:    start,
			Stop:     stop,
		}
	}

	s.Parts[0].Stop = start - 1

	return nil
}

func (s *Session) dropSkipped() {
	var skipped int
	for _, p := range s.Parts {
		if p.Skip {
			skipped++
		}
	}
	if skipped == len(s.Parts)-1 && !s.Parts[0].Skip {
		s.Parts = s.Parts[:1]
	}
}

func (s Session) concatenateParts(progress *mpb.Progress, logger *log.Logger) (err error) {
	if len(s.Parts) <= 1 {
		return nil
	}
	totalWritten := s.totalWritten()
	if totalWritten != s.ContentLength {
		return errors.Errorf("Size mismatch: expected %d got %d", s.ContentLength, totalWritten)
	}

	bar, err := progress.Add(int64(len(s.Parts)-1),
		mpb.BarStyle().Lbound(" ").Rbound(" ").Build(),
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
	fpart0, err := os.OpenFile(s.Parts[0].FileName, os.O_APPEND|os.O_WRONLY, 0644)
	if err != nil {
		return err
	}
	defer func() {
		if e := fpart0.Close(); err == nil {
			err = e
		}
		bar.Abort(false) // if bar is completed bar.Abort is nop
	}()

	for i := 1; i < len(s.Parts); i++ {
		if !s.Parts[i].Skip {
			fparti, err := os.Open(s.Parts[i].FileName)
			if err != nil {
				return err
			}
			logger.Printf("concatenating: %q into %q", fparti.Name(), fpart0.Name())
			_, err = io.Copy(fpart0, fparti)
			err = eitherError(err, fparti.Close())
			if err != nil {
				return err
			}
			err = os.Remove(fparti.Name())
			if err != nil {
				return err
			}
		}
		bar.Increment()
	}

	stat, err := fpart0.Stat()
	if err != nil {
		return err
	}
	if totalWritten != stat.Size() {
		panic("totalWritten != stat.Size()")
	}
	return nil
}

func (s *Session) loadState(fileName string) error {
	src, err := os.Open(fileName)
	if err != nil {
		return err
	}

	err = json.NewDecoder(bufio.NewReader(src)).Decode(s)
	if e := src.Close(); err == nil {
		err = e
	}
	return err
}

func (s Session) totalWritten() int64 {
	var total int64
	for _, p := range s.Parts {
		total += p.Written
	}
	return total
}

func (s Session) summary(logger *log.Logger) {
	format := fmt.Sprintf("Length: %%s [%s]", s.ContentType)
	if s.ContentLength <= 0 {
		logger.Printf(format, "unknown")
	} else {
		summary := fmt.Sprintf("%d (%.1f)", s.ContentLength, decor.SizeB1024(s.ContentLength))
		logger.Printf(format, summary)
		if total := s.totalWritten(); total != 0 {
			remaining := s.ContentLength - total
			logger.Printf("Remaining: %d (%.1f)", remaining, decor.SizeB1024(remaining))
		}
	}
	if len(s.Parts) != 0 {
		logger.Printf("Saving to: %q", s.OutputFileName)
	}
	if !s.isResumable() {
		prefix := logger.Prefix()
		logger.SetPrefix("[WARN] ")
		logger.Println("Session is not resumable")
		logger.SetPrefix(prefix)
	}
}

func (s Session) isOutputFileExist() (bool, error) {
	stat, err := os.Stat(s.OutputFileName)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return false, nil
		}
		return false, err
	}
	if stat.IsDir() {
		return true, errors.Wrapf(os.ErrInvalid, "%q is a directory", s.OutputFileName)
	}
	return true, nil
}

func (s Session) checkContentSums(other Session) error {
	if s.ContentLength != other.ContentLength {
		return fmt.Errorf(
			"ContentLength mismatch: expected %d got %d",
			s.ContentLength, other.ContentLength,
		)
	}
	if s.ContentMD5 != other.ContentMD5 {
		return fmt.Errorf(
			"%s mismatch: expected %q got %q",
			hContentMD5, s.ContentMD5, other.ContentMD5,
		)
	}
	return nil
}

func (s Session) checkSizeOfEachPart() error {
	for _, part := range s.Parts {
		if part.Written == 0 {
			continue
		}
		stat, err := os.Stat(part.FileName)
		if err != nil {
			return err
		}
		if fileSize := stat.Size(); part.Written != fileSize {
			return fmt.Errorf(
				"%q size mismatch: expected %d got %d",
				part.FileName, part.Written, fileSize,
			)
		}
	}
	return nil
}

func (s Session) makeTotalBar(
	ctx context.Context,
	progress *mpb.Progress,
	doneCount *uint32,
	quiet bool,
) (func(int, time.Duration), func(bool)) {
	if len(s.Parts) <= 1 || quiet {
		return func(int, time.Duration) {}, func(bool) {}
	}
	bar := progress.New(s.ContentLength, totalBarStyle(), mpb.BarFillerTrim(),
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
			decor.OnComplete(decor.AverageETA(decor.ET_STYLE_MMSS, decor.WCSyncWidth), ":"),
			decor.EwmaSpeed(decor.SizeB1024(0), "%.1f", 30, decor.WCSyncSpace),
			decor.Name("", decor.WCSyncSpace),
			decor.Name("", decor.WCSyncSpace),
		),
	)
	if written := s.totalWritten(); written != 0 {
		bar.SetCurrent(written)
		bar.SetRefill(written)
		bar.DecoratorAverageAdjust(time.Now().Add(-s.Elapsed))
	}
	type ewmaPayload struct {
		n int
		d time.Duration
	}
	ch := make(chan ewmaPayload, len(s.Parts)-int(atomic.LoadUint32(doneCount)))
	ctx, cancel := context.WithCancel(ctx)
	dropCtx, dropCancel := context.WithCancel(context.Background())
	go func() {
		for {
			select {
			case p := <-ch:
				bar.EwmaIncrBy(p.n, p.d)
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
	return func(n int, dur time.Duration) {
			ch <- ewmaPayload{n, dur}
		}, func(drop bool) {
			if drop {
				dropCancel()
			} else {
				cancel()
			}
		}
}

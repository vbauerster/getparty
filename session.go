package getparty

import (
	"bufio"
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

// not concurrency safe!
var totalWrittenCache map[time.Duration]int64 = make(map[time.Duration]int64)

// Session represents download session state
type Session struct {
	location          string
	URL               string
	SuggestedFileName string
	ContentMD5        string
	AcceptRanges      string
	ContentType       string
	StatusCode        int
	ContentLength     int64
	Redirected        bool
	Elapsed           time.Duration
	HeaderMap         map[string]string
	Parts             []*Part
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
		FileName: s.SuggestedFileName,
	}

	var stop int64
	start := s.ContentLength
	for i := parts - 1; i > 0; i-- {
		stop = start - 1
		start = stop - fragment
		s.Parts[i] = &Part{
			FileName: fmt.Sprintf("%s.%02d", s.SuggestedFileName, i),
			Start:    start,
			Stop:     stop,
		}
	}

	s.Parts[0].Stop = start - 1

	return nil
}

func (s Session) concatenateParts(dlogger *log.Logger, progress *mpb.Progress) error {
	if !s.isResumable() {
		return nil
	}
	fpart0, err := os.OpenFile(s.Parts[0].FileName, os.O_APPEND|os.O_WRONLY, 0644)
	if err != nil {
		return err
	}

	if len(s.Parts) > 1 {
		bar := progress.New(int64(len(s.Parts)-1),
			mpb.BarStyle().Lbound(" ").Rbound(" "),
			mpb.BarFillerTrim(),
			mpb.BarPriority(len(s.Parts)+1),
			mpb.PrependDecorators(
				decor.Name("Concatenating", decor.WCSyncWidthR),
				decor.NewPercentage("%d", decor.WCSyncSpace),
			),
			mpb.AppendDecorators(
				decor.OnComplete(decor.AverageETA(decor.ET_STYLE_MMSS), "Done"),
			),
		)
		defer bar.Abort(false) // if bar is completed bar.Abort is nop
		for i := 1; i < len(s.Parts); i++ {
			fparti, err := os.Open(s.Parts[i].FileName)
			if err != nil {
				return err
			}
			dlogger.Printf("concatenating: %s", fparti.Name())
			if _, err := io.Copy(fpart0, fparti); err != nil {
				return err
			}
			for _, err := range [...]error{fparti.Close(), os.Remove(fparti.Name())} {
				if err != nil {
					dlogger.Printf("concatenateParts: %q %s", fparti.Name(), err.Error())
				}
			}
			bar.Increment()
		}
	}

	stat, err := fpart0.Stat()
	if err != nil {
		return err
	}
	if size := stat.Size(); size != s.ContentLength {
		return errors.Errorf("Size mismatch: Expected=%d Saved=%d", s.ContentLength, size)
	}
	return fpart0.Close()
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
	total, ok := totalWrittenCache[s.Elapsed]
	if ok {
		return total
	}
	for _, p := range s.Parts {
		total += p.Written
	}
	totalWrittenCache[s.Elapsed] = total
	return total
}

func (s Session) writeSummary(w io.Writer, quiet bool) {
	if quiet {
		return
	}
	format := fmt.Sprintf("Length: %%s [%s]\n", s.ContentType)
	lengthSummary := "unknown"
	if s.ContentLength >= 0 {
		lengthSummary = fmt.Sprintf("%d (%.1f)", s.ContentLength, decor.SizeB1024(s.ContentLength))
		if written := s.totalWritten(); written > 0 {
			remaining := s.ContentLength - written
			lengthSummary += fmt.Sprintf(", %d (%.1f) remaining", remaining, decor.SizeB1024(remaining))
		}
	}
	fmt.Fprintf(w, format, lengthSummary)
	if s.ContentMD5 != "" {
		fmt.Fprintf(w, "MD5: %s\n", s.ContentMD5)
	}
	if !s.isResumable() {
		fmt.Fprintln(w, "HTTP server doesn't seem to support byte ranges. Cannot resume.")
	}
	if len(s.Parts) != 0 {
		fmt.Fprintf(w, "Saving to: %q\n\n", s.SuggestedFileName)
	}
}

func (s Session) checkExistingFile(w io.Writer, forceOverwrite bool) error {
	stat, err := os.Stat(s.SuggestedFileName)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return nil
		}
		return err
	}
	if stat.IsDir() {
		return fmt.Errorf("%v: %q is a directory", os.ErrInvalid, stat.Name())
	}
	if forceOverwrite {
		return os.Remove(s.SuggestedFileName)
	}
	var answer string
	fmt.Fprintf(w, "File %q already exists, overwrite? [y/n] ", stat.Name())
	if _, err := fmt.Scanf("%s", &answer); err != nil {
		return err
	}
	switch strings.ToLower(answer) {
	case "y", "yes":
		return os.Remove(s.SuggestedFileName)
	default:
		return ErrCanceledByUser
	}
}

func (s Session) checkSums(other *Session) error {
	if s.ContentMD5 != other.ContentMD5 {
		return fmt.Errorf(
			"ContentMD5 mismatch: expected %q got %q",
			s.ContentMD5, other.ContentMD5,
		)
	}
	if s.ContentLength != other.ContentLength {
		return fmt.Errorf(
			"ContentLength mismatch: expected %d got %d",
			s.ContentLength, other.ContentLength,
		)
	}
	return nil
}

func (s Session) checkPartsSize() error {
	for _, part := range s.Parts {
		if part.Skip {
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

func (s Session) makeTotalWriter(progress *mpb.Progress, partsDone *uint32, quiet bool) (io.Writer, func(bool)) {
	if len(s.Parts) <= 1 || quiet {
		return io.Discard, func(bool) {}
	}
	totalDecorator := func(_ decor.Statistics) string {
		return fmt.Sprintf("Total(%d/%d)", atomic.LoadUint32(partsDone), len(s.Parts))
	}
	bar := progress.New(s.ContentLength,
		totalBarStyle(),
		mpb.BarFillerTrim(),
		mpb.PrependDecorators(
			decor.Any(totalDecorator, decor.WCSyncWidthR),
			decor.OnComplete(decor.NewPercentage("%.2f", decor.WCSyncSpace), "100%"),
		),
		mpb.AppendDecorators(
			decor.OnComplete(decor.AverageETA(decor.ET_STYLE_MMSS, decor.WCSyncWidth), "Avg:"),
			decor.AverageSpeed(decor.UnitKiB, "%.1f", decor.WCSyncSpace),
		),
	)
	if written := s.totalWritten(); written > 0 {
		bar.SetCurrent(written)
		bar.SetRefill(written)
	}
	if s.Elapsed > 0 {
		bar.DecoratorAverageAdjust(time.Now().Add(-s.Elapsed))
	}
	return bar.ProxyWriter(io.Discard), func(drop bool) { bar.Abort(drop) }
}

package getparty

import (
	"cmp"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"os"
	"strings"
	"time"

	"github.com/vbauerster/mpb/v8"
	"github.com/vbauerster/mpb/v8/decor"
)

// Session represents download session state
type Session struct {
	dir           string
	location      string
	URL           string
	OutputName    string
	AcceptRanges  string
	ContentType   string
	ContentLength int64
	Elapsed       time.Duration
	Headers       map[string]string
	Parts         []*Part
	Single        bool
	restored      bool
	canceled      bool
}

func (s *Session) loadState(name string) error {
	f, err := os.Open(name)
	if err != nil {
		return err
	}
	return cmp.Or(json.NewDecoder(f).Decode(s), f.Close())
}

func (s *Session) dumpState(name string) error {
	f, err := os.Create(name)
	if err != nil {
		return err
	}
	return cmp.Or(json.NewEncoder(f).Encode(s), f.Close())
}

func (s *Session) cancel() {
	for _, p := range s.Parts {
		if p.cancel != nil {
			p.cancel()
		}
	}
	s.canceled = true
}

func (s Session) concatenate(progress *progress, logger *log.Logger) (*outFile, error) {
	if s.Single {
		return s.Parts[0].output, nil
	}
	if !s.isResumable() {
		return nil, errors.New("attempt to concat unresumable session")
	}
	bar, err := progress.addMergeBar(len(s.Parts))
	if err != nil {
		return nil, err
	}

	parts := make([]*outFile, 0, len(s.Parts))
	for _, p := range s.Parts {
		parts = append(parts, p.output)
	}

	err = concat(logger, bar, parts)
	if err != nil {
		bar.Abort(false)
		return nil, err
	}

	stat, err := parts[0].Stat()
	if err != nil {
		bar.Abort(false)
		return nil, err
	}

	if s.ContentLength != stat.Size() {
		err := ContentMismatchError[int64]{
			kind: "Length",
			old:  s.ContentLength,
			new:  stat.Size(),
		}
		bar.Abort(false)
		return nil, err
	}

	bar.Increment()
	return parts[0], nil
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

func (s Session) activePartsCount() int {
	count := len(s.Parts)
	for _, p := range s.Parts {
		if p.isContentDownloaded() {
			count--
		}
	}
	return count
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
		loggers[DBUG].Println(message)
	}
}

func (s Session) makeStateQuery() func(error) (int64, sessionState) {
	if !s.isResumable() {
		return func(err error) (int64, sessionState) {
			tw := s.totalWritten()
			if err != nil {
				return tw, sessionUncompleted
			}
			return tw, sessionCompleted
		}
	}
	initialWritten := s.totalWritten()
	return func(err error) (int64, sessionState) {
		tw := s.totalWritten()
		if tw != s.ContentLength {
			if tw != initialWritten { // if some bytes were written
				return tw, sessionUncompletedWithAdvance
			}
			return tw, sessionUncompleted
		}
		if err != nil {
			return tw, sessionCompletedWithError
		}
		return tw, sessionCompleted
	}
}

func (s Session) newProgress(ctx context.Context, out, err io.Writer) *progress {
	var total chan int
	if !s.Single {
		total = make(chan int, min(s.activePartsCount(), 12))
	}
	p := mpb.NewWithContext(ctx,
		mpb.WithOutput(out),
		mpb.WithDebugOutput(err),
		mpb.WithRefreshRate(refreshRate*time.Millisecond),
		mpb.WithWidth(64),
	)
	return &progress{
		Progress: p,
		topBar:   p.New(0, nil),
		total:    total,
		current:  s.totalWritten(),
		out:      out,
	}
}

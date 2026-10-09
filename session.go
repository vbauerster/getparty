package getparty

import (
	"cmp"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"os"
	"strings"
	"time"

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

func (s *Session) makeParts(n uint) error {
	if n == 0 {
		return ErrZeroParts
	}

	if !s.isResumable() {
		n = 1
	}

	fragment := s.ContentLength / int64(n)
	if n != 1 && fragment < minFragment {
		return ErrTooFragmented
	}

	parts := make([]*Part, 0, n)

	var offset int64
	for i := range n {
		p := &Part{
			Id:    i + 1,
			Start: offset,
			Stop:  offset + fragment - 1,
		}
		offset += p.len()
		parts = append(parts, p)
	}

	if offset != s.ContentLength {
		parts[n-1].Stop = s.ContentLength - 1
	}

	s.Parts = parts
	s.Single = n == 1
	return nil
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

func (s Session) concatenate(progress *progress, logger *log.Logger) (of *outFile, err error) {
	var stat os.FileInfo
	parts := make([]*outFile, 0, len(s.Parts))
	for _, p := range s.Parts {
		p.output.written = p.Written
		parts = append(parts, p.output)
	}

	defer func() {
		if err != nil {
			return
		}
		stat, err = of.Stat()
		if err != nil {
			return
		}
		if size := stat.Size(); s.ContentLength != size {
			err = ContentMismatchError[int64]{
				kind: "Length",
				old:  s.ContentLength,
				new:  size,
			}
		}
	}()

	if s.Single {
		if !s.isResumable() {
			s.ContentLength = parts[0].written
		}
		return parts[0], nil
	}
	if !s.isResumable() {
		return nil, errors.New("attempt to concat unresumable session")
	}

	bar, err := progress.addMergeBar(len(parts))
	if err != nil {
		return nil, err
	}
	defer func() {
		if err != nil {
			bar.Abort(false)
		} else {
			bar.Increment()
		}
	}()

	for _, of := range parts {
		err := of.checkWritten()
		if err != nil {
			return nil, err
		}
	}

	return parts[0], concat(logger, bar, parts)
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

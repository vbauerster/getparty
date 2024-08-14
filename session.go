package getparty

import (
	"encoding/json"
	"fmt"
	"log"
	"os"
	"strings"
	"time"

	"github.com/pkg/errors"
	"github.com/vbauerster/mpb/v8/decor"
)

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

	// if session isn't resumable stop is always negative
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
	return strings.EqualFold(s.AcceptRanges, "bytes") && s.ContentLength >= 0
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
	switch {
	case s.isResumable():
		summary := fmt.Sprintf("%d (%.1f)", s.ContentLength, decor.SizeB1024(s.ContentLength))
		loggers[INFO].Printf(format, summary)
		if total := s.totalWritten(); total != 0 {
			remaining := s.ContentLength - total
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
	single := len(s.Parts) == 1
	for i, p := range s.Parts {
		if p.Written == 0 {
			continue
		}
		p.order = i + 1
		p.single = single
		stat, err := os.Stat(p.outputName(s.OutputName))
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

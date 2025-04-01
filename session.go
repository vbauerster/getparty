package getparty

import (
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

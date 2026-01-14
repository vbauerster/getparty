package getparty

import (
	"cmp"
	"encoding/json"
	"fmt"
	"log"
	"os"
	"strings"
	"time"

	"github.com/vbauerster/mpb/v8/decor"
)

// Session represents download session state
type Session struct {
	URL           string
	OutputName    string
	AcceptRanges  string
	ContentType   string
	StatusCode    int
	ContentLength int64
	Elapsed       time.Duration
	HeaderMap     map[string]string
	Parts         []*Part
	Single        bool

	restored bool
	location string
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

func (s Session) summary(loggers [lEVELS]*log.Logger, saving bool) {
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
	if saving {
		loggers[INFO].Printf("Saving to: %q", s.OutputName)
	}
}

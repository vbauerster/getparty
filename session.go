package getparty

import (
	"encoding/json"
	"fmt"
	"io"
	"log"
	"os"

	"github.com/vbauerster/mpb/v4"
	"github.com/vbauerster/mpb/v4/decor"
)

// Session represents download session state
type Session struct {
	Location          string
	SuggestedFileName string
	ContentMD5        string
	AcceptRanges      string
	StatusCode        int
	ContentLength     int64
	ContentType       string
	HeaderMap         map[string]string
	Parts             []*Part
}

func (s Session) calcParts(parts int64) []*Part {
	var partSize int64
	if s.ContentLength <= 0 {
		parts = 1
	} else {
		partSize = s.ContentLength / parts
	}

	ps := make([]*Part, parts)
	ps[0] = &Part{
		FileName: s.SuggestedFileName,
	}

	stop := s.ContentLength
	start := stop
	for i := parts - 1; i > 0; i-- {
		stop = start - 1
		start = stop - partSize
		ps[i] = &Part{
			FileName: fmt.Sprintf("%s.part%d", s.SuggestedFileName, i),
			Start:    start,
			Stop:     stop,
		}
	}

	stop = start - 1
	if stop < parts*8 {
		return ps[:1]
	}

	ps[0].Stop = stop
	return ps
}

func (s Session) concatenateParts(dlogger *log.Logger, progress *mpb.Progress) (err error) {
	if len(s.Parts) <= 1 {
		return nil
	}

	fpart0, err := os.OpenFile(s.Parts[0].FileName, os.O_APPEND|os.O_WRONLY, 0644)
	if err != nil {
		return err
	}

	name := "concatenating parts:"
	bar := progress.AddBar(int64(len(s.Parts)-1), mpb.BarStyle("[=>-|"),
		mpb.BarPriority(len(s.Parts)),
		mpb.PrependDecorators(
			decor.Name(name),
			pad(len(name)-6, decor.WCSyncWidth),
		),
		mpb.AppendDecorators(
			decor.OnComplete(decor.AverageETA(decor.ET_STYLE_MMSS), "done!"),
			decor.Name(" ] "),
			decor.Percentage(),
		),
	)
	defer func() {
		if err != nil {
			bar.Abort(false)
		}
	}()

	dlogger.Printf("concatenating: %s", fpart0.Name())
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
				dlogger.Printf("concatenateParts: %q %v", fparti.Name(), err)
			}
		}
		bar.Increment()
	}
	return fpart0.Close()
}

func (s *Session) saveState(fileName string) error {
	dst, err := os.Create(fileName)
	if err != nil {
		return err
	}

	err = json.NewEncoder(dst).Encode(s)
	if e := dst.Close(); err == nil {
		err = e
	}
	return err
}

func (s *Session) loadState(fileName string) error {
	src, err := os.Open(fileName)
	if err != nil {
		return err
	}

	err = json.NewDecoder(src).Decode(s)
	if e := src.Close(); err == nil {
		err = e
	}
	return err
}

func (s *Session) actualPartsOnly() {
	parts := s.Parts[:0]
	for _, p := range s.Parts {
		if p.Skip {
			continue
		}
		parts = append(parts, p)
	}
	s.Parts = parts
}

func (s Session) totalWritten() int64 {
	var total int64
	for _, p := range s.Parts {
		total += p.Written
	}
	return total
}

func (s Session) writeSummary(w io.Writer) {
	humanSize := decor.CounterKiB(s.ContentLength)
	format := fmt.Sprintf("Length: %%s [%s]\n", s.ContentType)
	lengthSummary := "unknown"
	if s.ContentLength >= 0 {
		lengthSummary = fmt.Sprintf("%d (% .2f)", s.ContentLength, humanSize)
		if totalWritten := s.totalWritten(); totalWritten > 0 {
			remaining := s.ContentLength - totalWritten
			lengthSummary += fmt.Sprintf(", %d (% .2f) remaining", remaining, decor.CounterKiB(remaining))
		}
	}
	fmt.Fprintf(w, format, lengthSummary)
	if s.ContentMD5 != "" {
		fmt.Fprintf(w, "MD5: %s\n", s.ContentMD5)
	}
	switch s.AcceptRanges {
	case "", "none":
		fmt.Fprintln(w, "Looks like server doesn't support range requests (no party, no resume)")
	}
	fmt.Fprintf(w, "Saving to: %q\n\n", s.SuggestedFileName)
}

func (s Session) removeFiles() (err error) {
	for _, part := range s.Parts {
		if e := os.Remove(part.FileName); err == nil && !os.IsNotExist(e) {
			err = e
		}
	}
	return err
}

package getparty

import (
	"encoding/json"
	"fmt"
	"io"
	"log"
	"os"

	"github.com/vbauerster/mpb"
	"github.com/vbauerster/mpb/decor"
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
	Parts             []*Part
}

func (s Session) calcParts(parts int64) []*Part {
	if parts == 0 || s.ContentLength <= 0 {
		parts = 1
	}

	partSize := s.ContentLength / parts
	ps := make([]*Part, parts)
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
	ps[0] = &Part{
		FileName: s.SuggestedFileName,
		Stop:     stop,
	}
	if stop < 0 || stop < parts*8 {
		ps[0].Stop = 0
		return ps[:1:1]
	}
	return ps
}

func (s Session) concatenateParts(dlogger *log.Logger, pb *mpb.Progress) error {
	fpart0, err := os.OpenFile(s.Parts[0].FileName, os.O_APPEND|os.O_WRONLY, 0644)
	if err != nil {
		return err
	}

	var bar *mpb.Bar
	if len(s.Parts) > 1 {
		name := "concatenating parts:"
		bar = pb.AddBar(int64(len(s.Parts)-1), mpb.BarPriority(len(s.Parts)),
			mpb.BarRemoveOnComplete(),
			mpb.PrependDecorators(
				decor.Name(name),
				padDecorator(len(name)-6),
			),
			mpb.AppendDecorators(
				decor.OnComplete(decor.EwmaETA(decor.ET_STYLE_MMSS, 30), "done!"),
				decor.Name(" ] "),
				decor.Percentage(decor.WCSyncWidthR),
			),
		)
	}

	for i := 1; i < len(s.Parts); i++ {
		if s.Parts[i].Skip {
			bar.Increment()
			continue
		}
		fparti, err := os.Open(s.Parts[i].FileName)
		if err != nil {
			return err
		}
		if _, err := io.Copy(fpart0, fparti); err != nil {
			return err
		}
		for _, err := range [...]error{fparti.Close(), os.Remove(fparti.Name())} {
			if err != nil {
				dlogger.Printf("concatenateParts: %q %v\n", fparti.Name(), err)
			}
		}
		bar.Increment()
	}
	return fpart0.Close()
}

func (s Session) marshalState() (string, error) {
	name := s.SuggestedFileName + ".json"
	data, err := json.Marshal(s)
	if err != nil {
		return name, err
	}
	dst, err := os.Create(name)
	if err != nil {
		return name, err
	}
	_, err = dst.Write(data)
	if errc := dst.Close(); err == nil {
		err = errc
	}
	return name, err
}

func (s Session) totalWritten() int64 {
	var total int64
	for _, p := range s.Parts {
		if p.Skip {
			continue
		}
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

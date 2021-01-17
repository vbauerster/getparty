package getparty

import (
	"encoding/json"
	"fmt"
	"io"
	"log"
	"os"
	"strings"

	"github.com/pkg/errors"
	"github.com/vbauerster/mpb/v6"
	"github.com/vbauerster/mpb/v6/decor"
)

const (
	acceptRangesType = "bytes"
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

func (s Session) isAcceptRanges() bool {
	return strings.EqualFold(s.AcceptRanges, acceptRangesType)
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
	start := s.ContentLength
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
		// fragments are too small, so return as single part
		// no need to set *Part.Stop here, it's handled at *Part.getRange()
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

	nlOnComplete := func(w io.Writer, _ int, s decor.Statistics) {
		if s.Completed {
			fmt.Fprintln(w)
		}
	}

	bar := progress.Add(int64(len(s.Parts)-1),
		mpb.NewBarFiller(" =>- "),
		mpb.BarFillerTrim(),
		mpb.BarExtender(mpb.BarFillerFunc(nlOnComplete)),
		mpb.PrependDecorators(
			decor.Name("Concatenating:", decor.WCSyncWidthR),
			decor.NewPercentage("%d", decor.WCSyncSpace),
		),
		mpb.AppendDecorators(
			decor.OnComplete(
				decor.AverageETA(decor.ET_STYLE_MMSS, decor.WCSyncWidthR),
				"Done",
			),
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

func (s Session) totalWritten() int64 {
	var total int64
	for _, p := range s.Parts {
		total += p.Written
	}
	return total
}

func (s Session) writeSummary(w io.Writer) {
	humanSize := decor.SizeB1024(s.ContentLength)
	format := fmt.Sprintf("Length: %%s [%s]\n", s.ContentType)
	lengthSummary := "unknown"
	if s.ContentLength >= 0 {
		lengthSummary = fmt.Sprintf("%d (%.1f)", s.ContentLength, humanSize)
		if totalWritten := s.totalWritten(); totalWritten > 0 {
			remaining := s.ContentLength - totalWritten
			lengthSummary += fmt.Sprintf(", %d (%.1f) remaining", remaining, decor.SizeB1024(remaining))
		}
	}
	fmt.Fprintf(w, format, lengthSummary)
	if s.ContentMD5 != "" {
		fmt.Fprintf(w, "MD5: %s\n", s.ContentMD5)
	}
	if !s.isAcceptRanges() {
		fmt.Fprintln(w, "HTTP server doesn't seem to support byte ranges. Cannot resume.")
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

func (s Session) checkExistingFile(w io.Writer, forceOverwrite bool) error {
	stat, err := os.Stat(s.SuggestedFileName)
	if err != nil {
		if os.IsNotExist(err) {
			return nil
		}
		return err
	}
	if stat.IsDir() {
		return errors.Errorf("%q is a directory", stat.Name())
	}
	if forceOverwrite {
		return s.removeFiles()
	} else {
		var answer string
		fmt.Fprintf(w, "File %q already exists, overwrite? [y/n] ", stat.Name())
		if _, err := fmt.Scanf("%s", &answer); err != nil {
			return err
		}
		switch strings.ToLower(answer) {
		case "y", "yes":
			return s.removeFiles()
		default:
			return ErrCanceledByUser
		}
	}
}

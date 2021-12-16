package getparty

import (
	"bufio"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"os"
	"strings"

	"github.com/pkg/errors"
	"github.com/vbauerster/mpb/v7"
	"github.com/vbauerster/mpb/v7/decor"
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
	return strings.EqualFold(s.AcceptRanges, "bytes")
}

func (s Session) calcParts(dlogger *log.Logger, parts uint) []*Part {
	if !s.isAcceptRanges() || s.ContentLength <= 0 || parts == 0 {
		parts = 1
	}

	ps := make([]*Part, parts)
	ps[0] = &Part{
		FileName: s.SuggestedFileName,
	}

	stop := s.ContentLength
	start := s.ContentLength
	fragment := s.ContentLength / int64(parts)
	for i := parts - 1; i > 0; i-- {
		stop = start - 1
		start = stop - fragment
		ps[i] = &Part{
			FileName: fmt.Sprintf("%s.%02d", s.SuggestedFileName, i),
			Start:    start,
			Stop:     stop,
		}
	}

	ps[0].Stop = start - 1

	if parts > 1 && ps[0].Stop < int64(parts*8) {
		dlogger.Printf("too many parts (%d) for ContentLength=%d", parts, s.ContentLength)
		for i, p := range ps {
			dlogger.Printf("  fragment %02d: %d", i, p.Stop-p.Start)
		}
		ps[0].Stop = s.ContentLength - 1
		return ps[:1]
	}

	return ps
}

func (s Session) concatenateParts(dlogger *log.Logger, progress *mpb.Progress) (size int64, err error) {
	fpart0, err := os.OpenFile(s.Parts[0].FileName, os.O_APPEND|os.O_WRONLY, 0644)
	if err != nil {
		return 0, err
	}

	if len(s.Parts) > 1 {
		bar := progress.New(int64(len(s.Parts)-1),
			mpb.BarStyle().Lbound(" ").Rbound(" "),
			mpb.BarFillerTrim(),
			mpb.BarPriority(len(s.Parts)),
			mpb.PrependDecorators(
				decor.Name("Concatenating:", decor.WCSyncWidthR),
				decor.NewPercentage("%d", decor.WCSyncSpace),
			),
			mpb.AppendDecorators(
				decor.OnComplete(decor.AverageETA(decor.ET_STYLE_MMSS), "Done"),
			),
		)
		defer func() {
			if err != nil {
				bar.Abort(false)
			}
		}()
		for i := 1; i < len(s.Parts); i++ {
			fparti, err := os.Open(s.Parts[i].FileName)
			if err != nil {
				return 0, err
			}
			dlogger.Printf("concatenating: %s", fparti.Name())
			if _, err := io.Copy(fpart0, fparti); err != nil {
				return 0, err
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
		return 0, err
	}
	return stat.Size(), fpart0.Close()
}

func (s *Session) dumpState(w io.Writer) error {
	return json.NewEncoder(w).Encode(s)
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

func (s Session) writeSummary(w io.Writer, quiet bool) {
	if quiet {
		return
	}
	format := fmt.Sprintf("Length: %%s [%s]\n", s.ContentType)
	lengthSummary := "unknown"
	if s.ContentLength >= 0 {
		lengthSummary = fmt.Sprintf("%d (%.1f)", s.ContentLength, decor.SizeB1024(s.ContentLength))
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
		err = os.Remove(part.FileName)
		if errors.Is(err, os.ErrNotExist) {
			err = nil
		} else {
			break
		}
	}
	return err
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
		return s.removeFiles()
	}
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

package gp

import (
	"encoding/json"
	"fmt"
	"io"
	"log"
	"os"

	"github.com/vbauerster/mpb/decor"
)

// ActualLocation represents server's status 200 or 206 response meta data
// It never holds redirect responses
type ActualLocation struct {
	Location          string
	SuggestedFileName string
	ContentMD5        string
	AcceptRanges      string
	StatusCode        int
	ContentLength     int64
	ContentType       string
	Parts             []*Part
}

func (al *ActualLocation) calcParts(parts int) {
	if parts == 0 {
		return
	}
	partSize := al.ContentLength / int64(parts)
	if partSize <= 0 {
		parts = 1
	}
	al.Parts = make([]*Part, parts)

	stop := al.ContentLength
	start := stop
	for i := parts - 1; i > 0; i-- {
		stop = start - 1
		start = stop - partSize
		al.Parts[i] = &Part{
			FileName: fmt.Sprintf("%s.part%d", al.SuggestedFileName, i),
			Start:    start,
			Stop:     stop,
		}
	}
	stop = start - 1
	if stop < 0 {
		stop = 0
	}
	al.Parts[0] = &Part{
		FileName: al.SuggestedFileName,
		Stop:     stop,
	}
}

func (al *ActualLocation) concatenateParts(errLogger *log.Logger) error {
	fpart0, err := os.OpenFile(al.Parts[0].FileName, os.O_APPEND|os.O_WRONLY, 0644)
	if err != nil {
		return err
	}

	buf := make([]byte, 2*1024)
	for i := 1; i < len(al.Parts); i++ {
		fparti, err := os.Open(al.Parts[i].FileName)
		if err != nil {
			return err
		}
		for {
			n, err := fparti.Read(buf[:])
			_, errw := fpart0.Write(buf[:n])
			if errw != nil {
				return err
			}
			if err != nil {
				if err == io.EOF {
					break
				}
				return err
			}
		}
		for _, err := range [...]error{fparti.Close(), os.Remove(al.Parts[i].FileName)} {
			if err != nil {
				errLogger.Printf("concatenateParts: %v\n", err)
			}
		}
	}
	return fpart0.Close()
}

func (al *ActualLocation) deleteUnnecessaryParts() {
	for i := len(al.Parts) - 1; i >= 0; i-- {
		if al.Parts[i].Skip {
			al.Parts = append(al.Parts[:i], al.Parts[i+1:]...)
		}
	}
}

func (al *ActualLocation) marshalState(userURL string) (string, error) {
	name := al.SuggestedFileName + ".json"
	al.Location = userURL // preserve user provided url
	data, err := json.Marshal(al)
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

func (al *ActualLocation) totalWritten() int64 {
	var total int64
	if al == nil {
		return total
	}
	for _, p := range al.Parts {
		total += p.Written
	}
	return total
}

func (al *ActualLocation) writeSummary(w io.Writer) {
	humanSize := decor.CounterKiB(al.ContentLength)
	format := fmt.Sprintf("Length: %%s [%s]\n", al.ContentType)
	var length string
	totalWritten := al.totalWritten()
	if totalWritten > 0 && al.AcceptRanges != "" {
		remaining := al.ContentLength - totalWritten
		length = fmt.Sprintf("%d (% .2f), %d (% .2f) remaining",
			al.ContentLength,
			humanSize,
			remaining,
			decor.CounterKiB(remaining))
	} else if al.ContentLength < 0 {
		length = "unknown"
	} else {
		length = fmt.Sprintf("%d (% .2f)", al.ContentLength, humanSize)
	}
	fmt.Fprintf(w, format, length)
	if al.AcceptRanges == "" {
		fmt.Fprintln(w, "Looks like server doesn't support ranges (no party, no resume)")
	}
	fmt.Fprintf(w, "Saving to: %q\n\n", al.SuggestedFileName)
}

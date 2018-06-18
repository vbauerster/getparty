package getparty

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

func (al *ActualLocation) calcParts(parts int64) []*Part {
	if parts == 0 {
		parts = 1
	}
	partSize := al.ContentLength / int64(parts)
	if partSize <= 0 {
		parts = 1
	}

	ps := make([]*Part, parts)
	stop := al.ContentLength
	start := stop
	for i := parts - 1; i > 0; i-- {
		stop = start - 1
		start = stop - partSize
		ps[i] = &Part{
			FileName: fmt.Sprintf("%s.part%d", al.SuggestedFileName, i),
			Start:    start,
			Stop:     stop,
		}
	}
	stop = start - 1
	ps[0] = &Part{
		FileName: al.SuggestedFileName,
		Stop:     stop,
	}
	if stop < 0 || stop < parts*8 {
		ps[0].Stop = 0
		return ps[:1:1]
	}
	return ps
}

func (al *ActualLocation) concatenateParts(dlogger *log.Logger) error {
	fpart0, err := os.OpenFile(al.Parts[0].FileName, os.O_APPEND|os.O_WRONLY, 0644)
	if err != nil {
		return err
	}

	for i := 1; i < len(al.Parts); i++ {
		if al.Parts[i].Skip {
			continue
		}
		fparti, err := os.Open(al.Parts[i].FileName)
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
	}
	return fpart0.Close()
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
		if p.Skip {
			continue
		}
		total += p.Written
	}
	return total
}

func (al *ActualLocation) writeSummary(w io.Writer) {
	humanSize := decor.CounterKiB(al.ContentLength)
	format := fmt.Sprintf("Length: %%s [%s]\n", al.ContentType)
	lengthSummary := "unknown"
	if al.ContentLength >= 0 {
		lengthSummary = fmt.Sprintf("%d (% .2f)", al.ContentLength, humanSize)
		if totalWritten := al.totalWritten(); totalWritten > 0 {
			remaining := al.ContentLength - totalWritten
			lengthSummary += fmt.Sprintf(", %d (% .2f) remaining", remaining, decor.CounterKiB(remaining))
		}
	}
	fmt.Fprintf(w, format, lengthSummary)
	switch al.AcceptRanges {
	case "", "none":
		fmt.Fprintln(w, "Looks like server doesn't support range requests (no party, no resume)")
	}
	fmt.Fprintf(w, "Saving to: %q\n\n", al.SuggestedFileName)
}

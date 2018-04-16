package gp

import (
	"context"
	"fmt"
	"io"
	"log"
	"net/http"
	"os"

	"github.com/pkg/errors"
	rhttp "github.com/vbauerster/go-retryablehttp"
	"github.com/vbauerster/mpb"
	"github.com/vbauerster/mpb/decor"
)

// Part represents state of each download part
type Part struct {
	FileName             string
	Start, Stop, Written int64
	Skip                 bool
}

func (p *Part) download(ctx context.Context, dlogger *log.Logger, pb *mpb.Progress, rawUrl string, n int) error {
	if p.Stop-p.Start == p.Written-1 {
		return nil
	}

	pname := fmt.Sprintf("p#%02d:", n+1)

	req, err := rhttp.NewRequest(http.MethodGet, rawUrl, nil)
	if err != nil {
		return err
	}
	req = req.WithContext(ctx)
	req.Header.Set("User-Agent", userAgent)
	req.Header.Set("Range", p.getRange())

	messageCh := make(chan string, 1)
	client := rhttp.NewClient()
	client.Logger = dlogger
	client.Logger.Printf("%s %#v", pname, p)
	client.RequestLogHook = func(_ *log.Logger, _ *http.Request, i int) {
		if i == 0 {
			return
		}
		messageCh <- fmt.Sprintf("Retrying (%d)", i)
	}

	resp, err := client.Do(req)
	if err != nil {
		return err
	}
	defer func() {
		if resp.Body != nil {
			if err := resp.Body.Close(); err != nil {
				dlogger.Printf("%s %s resp.Body.Close() failed: %v", pname, rawUrl, err)
			}
		}
	}()

	total := p.Stop - p.Start + 1
	if resp.StatusCode == http.StatusOK {
		// no partial content, so try to download with single part
		if n > 0 {
			p.Skip = true
			return nil
		}
		total = resp.ContentLength
		p.Stop = total - 1
		p.Written = 0
	} else if resp.StatusCode != http.StatusPartialContent {
		return errors.Wrap(Error{
			errors.Errorf("%s unprocessable http status %q", pname, resp.Status),
		}, "download")
	}

	bar := pb.AddBar(total,
		mpb.PrependDecorators(
			decor.StaticName(pname, 0, 0),
			countersDecorator(messageCh, 6, 18),
		),
		mpb.AppendDecorators(speedDecorator()),
	)
	pb.UpdateBarPriority(bar, n)

	var dst *os.File
	if p.Written > 0 {
		dst, err = os.OpenFile(p.FileName, os.O_APPEND|os.O_WRONLY, 0644)
		bar.ResumeFill('+', p.Written)
		bar.IncrBy(int(p.Written))
	} else {
		dst, err = os.Create(p.FileName)
	}
	if err != nil {
		return errors.WithMessage(
			errors.Wrapf(Error{err}, "%s unable to write to %q", pname, p.FileName),
			"download",
		)
	}
	defer func() {
		if err := dst.Close(); err != nil {
			dlogger.Printf("%s closing %q failed: %v", pname, p.FileName, err)
		}
	}()

	reader := bar.ProxyReader(resp.Body)
	written, err := io.Copy(dst, reader)
	p.Written += written
	return err
}

func (p *Part) getRange() string {
	if p.Stop <= 0 {
		return ""
	}
	start := p.Start
	if p.Written > 0 {
		start = start + p.Written
	}
	return fmt.Sprintf("bytes=%d-%d", start, p.Stop)
}

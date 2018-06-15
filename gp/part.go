package gp

import (
	"context"
	"fmt"
	"io"
	"log"
	"net/http"
	"net/url"
	"os"
	"time"

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

func (p *Part) download(ctx context.Context, pb *mpb.Progress, dlogger *log.Logger, userInfo *url.Userinfo, userAgent, targetUrl string, n int) error {
	if p.Stop-p.Start == p.Written-1 {
		return nil
	}

	req, err := rhttp.NewRequest(http.MethodGet, targetUrl, nil)
	if err != nil {
		return err
	}
	req.URL.User = userInfo
	req.Header.Set("User-Agent", userAgent)
	req.Header.Set("Range", p.getRange())

	client := rhttp.NewClient()
	client.Logger = dlogger

	messageCh := make(chan string, 1)
	client.RequestLogHook = func(_ *log.Logger, _ *http.Request, i int) {
		if i == 0 {
			return
		}
		messageCh <- fmt.Sprintf("Retrying (%d)", i)
	}

	dlogger.Println("User-Agent:", userAgent)
	dlogger.Println("Range:", req.Header.Get("Range"))

	resp, err := client.Do(req.WithContext(ctx))
	if err != nil {
		return err
	}

	defer func() {
		if resp.Body != nil {
			if err := resp.Body.Close(); err != nil {
				dlogger.Printf("%s resp.Body.Close() failed: %v", targetUrl, err)
			}
		}
	}()

	pname := fmt.Sprintf("p#%02d:", n+1)
	total := p.Stop - p.Start + 1
	if resp.StatusCode == http.StatusOK {
		// no partial content, so try to download with single part
		if n > 0 {
			p.Skip = true
			dlogger.Println("server doesn't support range requests, skipping...")
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

	sbEta := make(chan time.Time)
	sbSpeed := make(chan time.Time)
	bar := pb.AddBar(total, mpb.BarPriority(n),
		mpb.PrependDecorators(
			decor.Name(pname),
			countersDecorator(messageCh, 6, 18),
		),
		mpb.AppendDecorators(
			decor.ETA(decor.ET_STYLE_MMSS, 600, sbEta),
			decor.Name(" ]"),
			decor.SpeedKibiByte("% .2f", 600, sbSpeed, decor.WCSyncSpace),
		),
	)

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
			dlogger.Printf("closing %q failed: %v", p.FileName, err)
		}
	}()

	reader := bar.ProxyReader(resp.Body, sbEta, sbSpeed)
	written, err := io.Copy(dst, reader)
	p.Written += written
	return err
}

func (p *Part) getRange() string {
	if p.Stop <= 0 {
		return "bytes=0-"
	}
	start := p.Start
	if p.Written > 0 {
		start += p.Written
	}
	return fmt.Sprintf("bytes=%d-%d", start, p.Stop)
}

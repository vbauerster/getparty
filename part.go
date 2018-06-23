package getparty

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"log"
	"net/http"
	"net/url"
	"os"
	"time"

	cleanhttp "github.com/hashicorp/go-cleanhttp"
	"github.com/pkg/errors"
	"github.com/vbauerster/backoff"
	"github.com/vbauerster/mpb"
	"github.com/vbauerster/mpb/decor"
)

// Part represents state of each download part
type Part struct {
	FileName string
	Start    int64
	Stop     int64
	Written  int64
	Skip     bool
}

func (p *Part) download(ctx context.Context, pb *mpb.Progress, dlogger *log.Logger, userInfo *url.Userinfo, userAgent, targetUrl string, n int) (err error) {
	if p.Stop-p.Start == p.Written-1 {
		return nil
	}

	pname := fmt.Sprintf("p#%02d:", n+1)
	defer func() {
		// just add method name, without stack trace at the point
		err = errors.WithMessage(err, "download: "+pname[:len(pname)-1])
	}()

	fpart, err := os.OpenFile(p.FileName, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0644)
	if err != nil {
		return err
	}
	defer func() {
		if err := fpart.Close(); err != nil {
			dlogger.Printf("closing %q failed: %v\n", fpart.Name(), err)
		}
		if p.Skip {
			if err := os.Remove(fpart.Name()); err != nil {
				dlogger.Printf("remove %q failed: %v\n", fpart.Name(), err)
			}
		}
	}()

	var bar *mpb.Bar
	var messageCh chan string

	return try(func(attempt int) (retry bool, err error) {
		defer func() {
			if e := recover(); e != nil {
				dlogger.Printf("%#v\n", p)
				panic(e)
			}
		}()

		req, err := http.NewRequest(http.MethodGet, targetUrl, nil)
		if err != nil {
			return false, err
		}
		req.URL.User = userInfo
		req.Header.Set("User-Agent", userAgent)
		req.Header.Set("Range", p.getRange())
		dlogger.Println("User-Agent:", userAgent)
		dlogger.Println("Range:", req.Header.Get("Range"))

		cctx, cancel := context.WithCancel(ctx)
		timer := time.AfterFunc(10*time.Second, cancel)
		defer cancel()

		client := cleanhttp.DefaultPooledClient()
		resp, err := client.Do(req.WithContext(cctx))
		if err != nil {
			return false, err
		}

		defer func() {
			if resp.Body == nil {
				return
			}
			if err := resp.Body.Close(); err != nil {
				dlogger.Printf("%s resp.Body.Close() failed: %v\n", targetUrl, err)
				return
			}
			dlogger.Println("resp body closed")
		}()

		total := p.Stop - p.Start + 1
		if resp.StatusCode == http.StatusOK {
			// no partial content, so try to download with single part
			if n > 0 {
				p.Skip = true
				dlogger.Println("server doesn't support range requests, skipping...")
				return false, nil
			}
			total = resp.ContentLength
			p.Stop = total - 1
			p.Written = 0
		} else if resp.StatusCode != http.StatusPartialContent {
			return false, ExpectedError{errors.Errorf("unprocessable http status %q", resp.Status)}
		}

		if bar == nil {
			age := float64(total+2) / 3.0
			messageCh = make(chan string, 1)
			bar = pb.AddBar(total, mpb.BarPriority(n),
				mpb.PrependDecorators(
					decor.Name(pname),
					percentageWithSizeCounter(messageCh, 5),
				),
				mpb.AppendDecorators(
					decor.EwmaETA(decor.ET_STYLE_MMSS, age),
					decor.Name(" ]"),
					decor.AverageSpeed(decor.UnitKiB, "% .2f", decor.WCSyncSpace),
				),
			)
			if p.Written > 0 {
				bar.RefillBy(int(p.Written), '+')
			}
		}

		var size, written int64
		size = 2048
		buf := bytes.NewBuffer(make([]byte, 0, size))
		reader := bar.ProxyReader(resp.Body)

		max := size
		for timer.Reset(8 * time.Second) {
			written, err = io.CopyN(buf, reader, max)
			if err != nil {
				if err == io.EOF || ctx.Err() == context.Canceled {
					break
				}
				// try to continue on temp err
				if e, ok := err.(interface{ Temporary() bool }); ok && e.Temporary() {
					max -= written
					messageCh <- "temp error"
					continue
				}
				// retry
				timer.Stop()
				messageCh <- fmt.Sprintf("retry #%d", attempt)
				dur := backoff.DefaultStrategy.Backoff(attempt)
				dlogger.Printf("sleep %s, before attempt %d\n", dur, attempt)
				time.Sleep(dur)
				break
			}
			written, _ = io.Copy(fpart, buf)
			p.Written += written
			max = size
			if total <= 0 {
				bar.SetTotal(p.Written+size, false)
			}
		}

		written, _ = io.Copy(fpart, buf)
		p.Written += written
		if total <= 0 {
			bar.SetTotal(p.Written, true)
			bar.IncrBy(0)
			// don't retry if content length is unknown
			// even if io.EOF isn't reached.
			p.Stop = p.Written - 1
		}
		// don't retry on io.EOF or user context.Canceled
		if err == io.EOF || ctx.Err() == context.Canceled {
			return false, nil
		}
		retry = p.Stop-p.Start != p.Written-1
		dlogger.Printf("attempt: %d, retry: %t, err: %v\n", attempt, retry, err)
		return retry, err
	})
}

func (p *Part) getRange() string {
	if p.Stop <= 0 {
		return "bytes=0-"
	}
	// don't change p.Start for bar.RefillBy sake
	start := p.Start
	if p.Written > 0 {
		start += p.Written
	}
	return fmt.Sprintf("bytes=%d-%d", start, p.Stop)
}

func try(fn func(int) (bool, error)) error {
	var err error
	var cont bool
	attempt := 1
	for {
		cont, err = fn(attempt)
		if !cont || err == nil {
			break
		}
		attempt++
	}
	return err
}

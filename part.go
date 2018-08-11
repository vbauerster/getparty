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

	"github.com/VividCortex/ewma"
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
	if p.isDone() {
		return nil
	}

	pname := fmt.Sprintf("p#%02d:", n+1)
	defer func() {
		dlogger.Println("quit:", err)
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
		if ctx.Err() != nil {
			return false, ctx.Err()
		}
		defer func() {
			dlogger.Printf("written: %d\n", p.Written)
			dlogger.Printf("retry: %t\n", retry)
			if e := recover(); e != nil {
				dlogger.Printf("%#v\n", p)
				panic(e)
			}
		}()

		dlogger.Printf("attempt: %d\n", attempt)
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
			}
		}()

		dlogger.Printf("Status: %s\n", resp.Status)
		dlogger.Printf("ContentLength: %d\n", resp.ContentLength)

		total := p.Stop - p.Start + 1
		if resp.StatusCode == http.StatusOK {
			// no partial content, so download with single part
			if n > 0 {
				p.Skip = true
				dlogger.Println("No partial content, skipping...")
				return false, nil
			}
			total = resp.ContentLength
			dlogger.Printf("Reset last written: %d\n", p.Written)
			p.Written = 0
		} else if resp.StatusCode != http.StatusPartialContent {
			return false, ExpectedError{errors.Errorf("unprocessable http status %q", resp.Status)}
		}

		var bufSize int64 = 1024 * 4
		if bar == nil {
			dlogger.Printf("Part's total: %d\n", total)
			var etaAge float64
			if total <= bufSize {
				etaAge = float64(total)
			} else {
				etaAge = float64(total) / float64(bufSize)
			}
			messageCh = make(chan string, 1)
			bar = pb.AddBar(total, mpb.BarPriority(n),
				mpb.PrependDecorators(
					decor.Name(pname),
					percentageWithTotal("%.1f%% of % .1f", decor.WCSyncSpace, messageCh, 12),
				),
				mpb.AppendDecorators(
					decor.OnComplete(
						decor.MovingAverageETA(
							decor.ET_STYLE_MMSS,
							ewma.NewMovingAverage(etaAge),
							decor.FixedIntervalTimeNormalizer(42),
						),
						"done!",
					),
					decor.Name(" ]"),
					decor.AverageSpeed(decor.UnitKiB, "% .2f", decor.WCSyncSpace),
				),
			)
			if p.Written > 0 {
				bar.RefillBy(int(p.Written), '+')
			}
		}

		var written int64
		buf := bytes.NewBuffer(make([]byte, 0, bufSize))
		reader := bar.ProxyReader(resp.Body)

		max := bufSize
		for timer.Reset(8 * time.Second) {
			written, err = io.CopyN(buf, reader, max)
			if err != nil {
				dlogger.Printf("CopyN err: %v\n", err)
				if err == io.EOF {
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
				dlogger.Printf("sleep %s, before next attempt\n", dur)
				time.Sleep(dur)
				break
			}
			written, _ = io.Copy(fpart, buf)
			p.Written += written
			if total <= 0 {
				bar.SetTotal(p.Written+max*2, false)
			}
			max = bufSize
		}

		written, _ = io.Copy(fpart, buf)
		p.Written += written
		if total <= 0 {
			bar.SetTotal(p.Written, true)
		}
		if err == io.EOF {
			return false, nil
		}
		retry = !p.isDone()
		return retry, err
	})
}

func (p Part) getRange() string {
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

func (p Part) isDone() bool {
	return p.Stop-p.Start == p.Written-1
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

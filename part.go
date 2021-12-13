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
	"sync/atomic"
	"time"

	"github.com/pkg/errors"
	"github.com/vbauerster/backoff"
	"github.com/vbauerster/backoff/exponential"
	"github.com/vbauerster/mpb/v7"
	"github.com/vbauerster/mpb/v7/decor"
)

const (
	bufSize = 1 << 12
)

var globTry uint32

// Part represents state of each download part
type Part struct {
	FileName string
	Start    int64
	Stop     int64
	Written  int64
	Skip     bool
	Elapsed  time.Duration

	name      string
	order     int
	maxTry    int
	curTry    uint32
	single    bool
	quiet     bool
	jar       http.CookieJar
	transport *http.Transport
	dlogger   *log.Logger
}

func (p *Part) makeBar(progress *mpb.Progress, gate *msgGate, total int64) *mpb.Bar {
	p.dlogger.Printf("making bar with total: %d", total)
	nlOnComplete := func(w io.Writer, _ int, s decor.Statistics) {
		if s.Completed {
			fmt.Fprintln(w)
		}
	}
	bar := progress.Add(total,
		mpb.NewBarFiller(mpb.BarStyle().Lbound(" ").Rbound(" ")),
		mpb.BarFillerTrim(),
		mpb.BarPriority(p.order),
		mpb.BarOptional(mpb.BarExtender(mpb.BarFillerFunc(nlOnComplete)), p.single),
		mpb.PrependDecorators(
			newMainDecorator(&p.curTry, "%s %.1f", p.name, gate, decor.WCSyncWidthR),
			decor.OnComplete(decor.NewPercentage("%.2f", decor.WCSyncSpace), "100%"),
		),
		mpb.AppendDecorators(
			decor.OnComplete(
				decor.NewAverageETA(
					decor.ET_STYLE_MMSS,
					time.Now(),
					decor.FixedIntervalTimeNormalizer(60),
					decor.WCSyncWidthR,
				),
				"Avg:",
			),
			decor.AverageSpeed(decor.UnitKiB, "%.1f", decor.WCSyncSpace),
			decor.OnComplete(decor.Name("", decor.WCSyncSpace), "Peak:"),
			newSpeedPeak("%.1f", decor.WCSyncSpace),
		),
	)
	return bar
}

func (p *Part) download(ctx context.Context, progress *mpb.Progress, req *http.Request, timeout uint) (err error) {
	defer func() {
		err = errors.Wrap(err, p.name)
	}()

	fpart, err := os.OpenFile(p.FileName, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0644)
	if err != nil {
		return err
	}
	defer func() {
		if err := fpart.Close(); err != nil {
			p.dlogger.Printf("%q close error: %s", fpart.Name(), err.Error())
		}
		if p.Skip {
			if err := os.Remove(p.FileName); err != nil {
				p.dlogger.Printf("%q remove error: %s", fpart.Name(), err.Error())
			}
		}
	}()

	var bar *mpb.Bar
	barInitDone := make(chan struct{})
	mg := newMsgGate(p.quiet, p.name, 15)
	prefix := p.dlogger.Prefix()
	initialWritten := p.Written
	initialTimeout := timeout
	resetDur := time.Duration(2*timeout) * time.Second
	lStart := time.Time{}

	return backoff.Retry(ctx, exponential.New(exponential.WithBaseDelay(500*time.Millisecond)), resetDur,
		func(count int, now time.Time) (retry bool, err error) {
			if p.isDone() {
				panic(fmt.Sprintf("%s is engaged while being done", prefix))
			}
			defer func() {
				p.Elapsed += time.Since(now)
				lStart = now
				if !retry && err != nil && bar != nil {
					if err == ErrMaxRetry {
						mg.finalFlash(err.Error())
					}
					bar.Abort(false)
				}
			}()

			req.Header.Set(hRange, p.getRange())

			p.dlogger.SetPrefix(fmt.Sprintf("%s[%02d] ", prefix, count))
			p.dlogger.Printf("GET %q", req.URL)
			p.dlogger.Printf("%s: %s", hUserAgentKey, req.Header.Get(hUserAgentKey))
			p.dlogger.Printf("%s: %s", hRange, req.Header.Get(hRange))

			if count > 0 {
				if now.Sub(lStart) < resetDur {
					timeout += 5
				} else {
					timeout = initialTimeout
				}
				atomic.AddUint32(&globTry, 1)
				atomic.StoreUint32(&p.curTry, uint32(count))
			}

			ctxTimeout := time.Duration(timeout) * time.Second
			ctx, cancel := context.WithCancel(ctx)
			defer cancel()
			timer := time.AfterFunc(ctxTimeout, func() {
				cancel()
				// checking for bar != nil here is a data race
				// select to rescue
				select {
				case <-barInitDone:
					mg.flash("Timeout...")
				default:
				}
				p.dlogger.Printf("Timeout after: %v", ctxTimeout)
			})
			defer timer.Stop()

			client := &http.Client{
				Transport: p.transport,
				Jar:       p.jar,
			}
			resp, err := client.Do(req.WithContext(ctx))
			if err != nil {
				p.dlogger.Printf("Client.Do err: %s", err.Error())
				if count+1 == p.maxTry {
					return false, ErrMaxRetry
				}
				return true, err
			}

			p.dlogger.Printf("Status: %s", resp.Status)
			p.dlogger.Printf("ContentLength: %d", resp.ContentLength)
			if cookies := p.jar.Cookies(req.URL); len(cookies) != 0 {
				p.dlogger.Println("CookieJar:")
				for _, cookie := range cookies {
					p.dlogger.Printf("  %q", cookie)
				}
			}

			var noPartial bool
			switch resp.StatusCode {
			case http.StatusOK: // no partial content, so download with single part
				if p.order != 0 {
					p.Skip = true
					p.dlogger.Print("no partial content, skipping...")
					return false, nil
				}
				p.single = true
				noPartial = true
				p.Written = 0
				if resp.ContentLength > 0 {
					p.Stop = resp.ContentLength - 1
				}
			case http.StatusForbidden, http.StatusTooManyRequests:
				mg.finalFlash(resp.Status)
				fallthrough
			default:
				if resp.StatusCode != http.StatusPartialContent {
					return false, &HttpError{resp.StatusCode, resp.Status}
				}
			}

			if bar == nil {
				bar = p.makeBar(progress, mg, total)
				close(barInitDone)
			}

			body := bar.ProxyReader(resp.Body)
			defer body.Close()

			if p.Written > 0 {
				bar.SetRefill(p.Written)
				p.dlogger.Printf("set bar refill: %d", p.Written)
				if p.Written == initialWritten {
					bar.DecoratorAverageAdjust(time.Now().Add(-p.Elapsed))
					bar.IncrInt64(p.Written)
				}
			}

			var n int64
			pWrittenSnap := p.Written
			buf := bytes.NewBuffer(make([]byte, 0, bufSize))
			max := int64(bufSize)
			for timer.Reset(ctxTimeout) {
				n, err = io.CopyN(buf, body, max)
				if err != nil {
					p.dlogger.Printf("CopyN err: %s", err.Error())
					if e, ok := err.(*url.Error); ok {
						go mg.flash(fmt.Sprintf("%.30s..", e.Err.Error()))
						if e.Temporary() {
							max -= n
							time.Sleep(50 * time.Millisecond)
							continue
						}
					}
					break
				}
				n, _ = io.Copy(fpart, buf)
				p.Written += n
				if p.total() <= 0 {
					bar.SetTotal(p.Written+max, false)
				}
				max = bufSize
			}

			n, _ = io.Copy(fpart, buf)
			p.Written += n
			p.dlogger.Printf("written: %d", p.Written-pWrittenSnap)

			if err == io.EOF {
				if p.total() <= 0 {
					bar.SetTotal(p.Written, true)
				}
				return false, nil
			}

			if noPartial {
				bar.SetCurrent(0)
				bar.SetTotal(0, false)
				if e := fpart.Close(); e == nil {
					fpart, e = os.OpenFile(p.FileName, os.O_WRONLY|os.O_TRUNC, 0644)
					if e != nil {
						panic(e)
					}
				}
			}

			if count+1 == p.maxTry {
				return false, ErrMaxRetry
			}

			return true, err
		})
}

func (p Part) getRange() string {
	if p.Stop < 1 {
		return "bytes=0-"
	}
	return fmt.Sprintf("bytes=%d-%d", p.Start+p.Written, p.Stop)
}

func (p Part) isDone() bool {
	return p.Skip || (p.Written > 0 && p.Written == p.total())
}

func (p Part) total() int64 {
	return p.Stop - p.Start + 1
}

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
	"github.com/vbauerster/mpb/v6"
	"github.com/vbauerster/mpb/v6/decor"
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

func (p *Part) makeBar(progress *mpb.Progress, gate msgGate, total int64) *mpb.Bar {
	nlOnComplete := func(w io.Writer, _ int, s decor.Statistics) {
		if s.Completed {
			fmt.Fprintln(w)
		}
	}

	bar := progress.Add(total,
		mpb.NewBarFiller(" =>- "),
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
			if err := os.Remove(fpart.Name()); err != nil {
				p.dlogger.Printf("%q remove error: %s", fpart.Name(), err.Error())
			}
		}
	}()

	var bar *mpb.Bar
	mg := newMsgGate(p.name, p.quiet)
	total := p.Stop - p.Start + 1
	initialWritten := p.Written
	prefix := p.dlogger.Prefix()

	return backoff.Retry(ctx,
		exponential.New(exponential.WithBaseDelay(100*time.Millisecond)),
		30*time.Second,
		func(count int, now time.Time) (retry bool, err error) {
			if p.isDone() {
				panic(fmt.Sprintf("%s is engaged while being done", prefix))
			}
			defer func() {
				p.Elapsed += time.Since(now)
			}()

			req.Header.Set(hRange, p.getRange())

			p.dlogger.SetPrefix(fmt.Sprintf("%s[%02d] ", prefix, count))
			p.dlogger.Printf("GET %q", req.URL)
			p.dlogger.Printf("%s: %s", hUserAgentKey, req.Header.Get(hUserAgentKey))
			p.dlogger.Printf("%s: %s", hRange, req.Header.Get(hRange))

			ctxTimeout := time.Duration(timeout) * time.Second
			if count > 0 {
				ctxTimeout = time.Duration((1<<uint(count-1))*timeout) * time.Second
				if bound := 10 * time.Minute; ctxTimeout > bound {
					ctxTimeout = bound
				}
				atomic.AddUint32(&globTry, 1)
				atomic.StoreUint32(&p.curTry, uint32(count))
				mg.flash(&message{msg: "Retrying..."})
			}

			ctx, cancel := context.WithCancel(ctx)
			defer cancel()
			timer := time.AfterFunc(ctxTimeout, func() {
				msg := "Timeout..."
				mg.flash(&message{msg: msg})
				p.dlogger.Print(msg)
				cancel()
			})
			defer timer.Stop()

			client := &http.Client{
				Transport: p.transport,
				Jar:       p.jar,
			}
			resp, err := client.Do(req.WithContext(ctx))
			if err != nil {
				p.dlogger.Printf("Client.Do err: %s", err.Error())
				return count+1 != p.maxTry, err
			}

			p.dlogger.Printf("resp.Status: %s", resp.Status)
			p.dlogger.Printf("resp.ContentLength: %d", resp.ContentLength)
			if cookies := p.jar.Cookies(req.URL); len(cookies) != 0 {
				p.dlogger.Println("CookieJar:")
				for _, cookie := range cookies {
					p.dlogger.Printf("  %q", cookie)
				}
			}

			switch resp.StatusCode {
			case http.StatusOK: // no partial content, so download with single part
				if p.order != 0 {
					p.Skip = true
					p.dlogger.Print("no partial content, skipping...")
					return false, nil
				}
				p.Written = 0
				p.single = true
			case http.StatusForbidden, http.StatusTooManyRequests:
				flushed := make(chan struct{})
				mg.flash(&message{
					msg:   resp.Status,
					final: true,
					done:  flushed,
				})
				<-flushed
				fallthrough
			default:
				if resp.StatusCode != http.StatusPartialContent {
					if bar != nil {
						bar.Abort(false)
					}
					return false, errors.Errorf("unexpected status: %s", resp.Status)
				}
			}

			if p.single && resp.ContentLength > 0 {
				total = resp.ContentLength
				p.Stop = resp.ContentLength - 1
			}

			if bar == nil {
				bar = p.makeBar(progress, mg, total)
				p.dlogger.Printf("bar total: %d", total)
			}

			body := bar.ProxyReader(resp.Body)
			defer body.Close()

			if p.Written > 0 {
				p.dlogger.Printf("bar refill written: %d", p.Written)
				bar.SetRefill(p.Written)
				if p.Written-initialWritten == 0 {
					bar.DecoratorAverageAdjust(time.Now().Add(-p.Elapsed))
					bar.IncrInt64(p.Written)
				}
			}

			pWrittenSnap := p.Written
			buf, max := bytes.NewBuffer(make([]byte, 0, bufSize)), int64(bufSize)
			var n int64
			for timer.Reset(ctxTimeout) {
				n, err = io.CopyN(buf, body, max)
				if err != nil {
					p.dlogger.Printf("CopyN err: %s", err.Error())
					if e, ok := err.(*url.Error); ok {
						mg.flash(&message{
							msg: fmt.Sprintf("%.30s..", e.Err.Error()),
						})
						if e.Temporary() {
							max -= n
							continue
						}
					}
					break
				}
				n, _ = io.Copy(fpart, buf)
				p.Written += n
				if total < 1 {
					bar.SetTotal(p.Written+max*2, false)
				}
				max = bufSize
			}

			n, _ = io.Copy(fpart, buf)
			p.Written += n
			p.dlogger.Printf("written: %d", p.Written-pWrittenSnap)

			if total < 1 {
				p.Stop = p.Written - 1
			}

			if err == io.EOF {
				if p.Written == pWrittenSnap {
					err = errors.Errorf("%s zero written", prefix)
				} else {
					err = nil
				}
				return false, err
			}

			if count+1 == p.maxTry {
				flushed := make(chan struct{})
				mg.flash(&message{
					msg:   "max retry!",
					final: true,
					done:  flushed,
				})
				<-flushed
				bar.Abort(false)
				return false, ErrMaxRetry
			}

			return true, err
		})
}

func (p Part) getRange() string {
	if p.Stop <= 0 {
		return "bytes=0-"
	}
	return fmt.Sprintf("bytes=%d-%d", p.Start+p.Written, p.Stop)
}

func (p Part) isDone() bool {
	return p.Skip || (p.Written > 0 && p.Written > p.Stop-p.Start)
}

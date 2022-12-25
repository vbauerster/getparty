package getparty

import (
	"context"
	"fmt"
	"io"
	"log"
	"net/http"
	"os"
	"sync/atomic"
	"time"

	"github.com/pkg/errors"
	"github.com/vbauerster/backoff"
	"github.com/vbauerster/backoff/exponential"
	"github.com/vbauerster/mpb/v8"
	"github.com/vbauerster/mpb/v8/decor"
)

const (
	bufSize = 4096
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

	name        string
	order       int
	maxTry      uint
	quiet       bool
	totalWriter io.Writer
	totalCancel func(bool)
	client      *http.Client
	dlogger     *log.Logger
}

func (p Part) makeBar(progress *mpb.Progress, curTry *uint32, initDone chan struct{}) (*mpb.Bar, *msgGate) {
	defer close(initDone)
	total := p.total()
	if total < 0 {
		total = 0
	}
	p.dlogger.Printf("Setting bar total: %d", total)
	mg := newMsgGate(p.quiet, p.name, 15)
	bar := progress.New(total,
		mpb.BarFillerBuilderFunc(func() mpb.BarFiller {
			if total == 0 {
				return mpb.NopStyle().Build()
			}
			return mpb.BarStyle().Lbound(" ").Rbound(" ").Build()
		}),
		mpb.BarFillerTrim(),
		mpb.BarPriority(p.order),
		mpb.PrependDecorators(
			newMainDecorator(curTry, "%s %.1f", p.name, mg, decor.WCSyncWidthR),
			decor.Conditional(
				total == 0,
				decor.OnComplete(decor.Spinner([]string{`-`, `\`, `|`, `/`}, decor.WCSyncSpace), "100% "),
				decor.OnComplete(decor.NewPercentage("%.2f", decor.WCSyncSpace), "100%"),
			),
		),
		mpb.AppendDecorators(
			decor.OnComplete(
				decor.Conditional(
					total == 0,
					decor.Name(""),
					decor.OnAbort(
						decor.NewAverageETA(
							decor.ET_STYLE_MMSS,
							time.Now(),
							decor.FixedIntervalTimeNormalizer(30),
							decor.WCSyncWidth,
						), "--:--"),
				), "Avg:"),
			decor.AverageSpeed(decor.UnitKiB, "%.1f", decor.WCSyncSpace),
			decor.OnComplete(decor.Name("", decor.WCSyncSpace), "Peak:"),
			newSpeedPeak("%.1f", decor.WCSyncSpace),
		),
	)
	if p.Written > 0 {
		p.dlogger.Printf("Setting bar current: %d", p.Written)
		bar.SetCurrent(p.Written)
		bar.SetRefill(p.Written)
	}
	if p.Elapsed > 0 {
		p.dlogger.Printf("Setting bar DecoratorAverageAdjust: -%d (-%[1]s)", p.Elapsed)
		bar.DecoratorAverageAdjust(time.Now().Add(-p.Elapsed))
	}
	return bar, mg
}

func (p *Part) download(
	ctx context.Context,
	progress *mpb.Progress,
	req *http.Request,
	timeout uint,
) (err error) {
	fpart, err := os.OpenFile(p.FileName, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0644)
	if err != nil {
		return errors.WithMessage(err, p.name)
	}
	defer func() {
		if e := fpart.Close(); err == nil {
			err = e
		}
		if p.Skip && err == nil {
			p.dlogger.Printf("Removing: %q", fpart.Name())
			err = os.Remove(p.FileName)
		}
		err = errors.WithMessage(err, p.name)
	}()

	var bar *mpb.Bar
	var mg *msgGate
	var curTry uint32
	resetTimeout := timeout
	barInitDone := make(chan struct{})
	prefix := p.dlogger.Prefix()
	start := time.Now()

	return backoff.Retry(ctx, exponential.New(exponential.WithBaseDelay(500*time.Millisecond)),
		func(attempt uint, reset func()) (retry bool, err error) {
			writtenBefore := p.Written
			defer func() {
				attemptDur := time.Since(start)
				if diff := p.Written - writtenBefore; diff != 0 {
					reset()
					timeout = resetTimeout
					p.Elapsed += attemptDur
					p.dlogger.Printf("Wrote %d bytes", diff)
				} else if timeout < maxTimeout {
					timeout += 5
				}
				if retry {
					if attempt == 1 {
						atomic.StoreUint32(&globTry, 1)
					}
					atomic.StoreUint32(&curTry, uint32(attempt))
					p.dlogger.Printf("Retry reason: %v", err)
				}
				p.dlogger.Printf("Ran dur: %s", attemptDur)
				start = time.Now()
			}()

			req.Header.Set(hRange, p.getRange())

			p.dlogger.SetPrefix(fmt.Sprintf("%s[R%02d] ", prefix, attempt))
			p.dlogger.Printf("GET %q", req.URL)
			p.dlogger.Printf("%s: %s", hUserAgentKey, req.Header.Get(hUserAgentKey))
			p.dlogger.Printf("%s: %s", hRange, req.Header.Get(hRange))

			ctx, cancel := context.WithCancel(ctx)
			defer cancel()
			timer := time.AfterFunc(time.Duration(timeout)*time.Second, func() {
				cancel()
				// checking for mg != nil here is a data race
				select {
				case <-barInitDone:
					mg.flash("Timeout...")
				default:
				}
			})
			defer timer.Stop()

			resp, err := p.client.Do(req.WithContext(ctx))
			if err != nil {
				if bar == nil {
					bar, mg = p.makeBar(progress, &curTry, barInitDone)
				}
				if p.isMaxRetry(attempt) {
					go bar.Abort(false)
					p.dlogger.Println(ErrMaxRetry.Error())
					mg.finalFlash(ErrMaxRetry.Error())
					return false, errors.Wrap(ErrMaxRetry, err.Error())
				}
				return true, err
			}

			p.dlogger.Printf("HTTP status: %s", resp.Status)
			p.dlogger.Printf("ContentLength: %d", resp.ContentLength)

			if jar := p.client.Jar; jar != nil {
				if cookies := jar.Cookies(req.URL); len(cookies) != 0 {
					p.dlogger.Println("CookieJar:")
					for _, cookie := range cookies {
						p.dlogger.Printf("  %q", cookie)
					}
				}
			}

			switch resp.StatusCode {
			case http.StatusOK: // no partial content, download with single part
				if p.order != 1 {
					p.Skip = true
					p.dlogger.Println("Skip: no partial content")
					return false, nil
				}
				p.totalCancel(true) // single bar doesn't need total bar
				if resp.ContentLength > 0 {
					p.Stop = resp.ContentLength - 1
				}
				p.Written = 0
				defer func() {
					if err != nil {
						e := fpart.Close()
						if e != nil {
							panic(e)
						}
						if p.isMaxRetry(attempt) {
							return
						}
						fpart, e = os.OpenFile(p.FileName, os.O_WRONLY|os.O_TRUNC, 0644)
						if e != nil {
							panic(e)
						}
						bar.SetCurrent(0)
					}
				}()
			case http.StatusForbidden, http.StatusTooManyRequests:
				if mg != nil {
					mg.finalFlash(resp.Status)
				}
				fallthrough
			default:
				if resp.StatusCode != http.StatusPartialContent {
					return false, &HttpError{resp.StatusCode, resp.Status}
				}
			}

			if bar == nil {
				bar, mg = p.makeBar(progress, &curTry, barInitDone)
			} else if p.Written > 0 {
				p.dlogger.Printf("Setting bar refill: %d", p.Written)
				bar.SetRefill(p.Written)
			}

			body := bar.ProxyReader(resp.Body)
			defer body.Close()

			writer := io.MultiWriter(fpart, p.totalWriter)

			buf := make([]byte, bufSize)
			for n := 0; err == nil && timer.Reset(time.Duration(timeout)*time.Second); {
				n, err = io.ReadFull(body, buf)
				if err != nil {
					p.dlogger.Printf("io.ReadFull: %d %s", n, err.Error())
					if n > 0 {
						err = nil
					} else if err == io.ErrUnexpectedEOF {
						err = io.EOF
					}
				}
				n, e := writer.Write(buf[:n])
				if e != nil {
					panic(e)
				}
				p.Written += int64(n)
				if p.total() <= 0 {
					if err == io.EOF {
						p.Stop = p.Written - 1 // so p.isDone() retruns true
						bar.SetTotal(p.Written, true)
					} else {
						bar.SetTotal(p.Written, false)
					}
				}
			}

			if timer.Stop() {
				cancel()
				p.dlogger.Println(ctx.Err())
			}

			if p.isDone() {
				p.dlogger.Printf("Done result: %v", err)
				if err == nil || err == io.EOF {
					return false, nil
				}
				panic(fmt.Sprintf("Part is done with unexpected err: %s", err.Error()))
			}

			if p.isMaxRetry(attempt) {
				go bar.Abort(false)
				p.dlogger.Println(ErrMaxRetry.Error())
				mg.finalFlash(ErrMaxRetry.Error())
				return false, ErrMaxRetry
			}

			for i := 0; err == nil; i++ {
				if i != 0 {
					panic("cannot retry with nil error")
				}
				err = ctx.Err()
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

func (p Part) total() int64 {
	return p.Stop - p.Start + 1
}

func (p Part) isDone() bool {
	return p.Written > 0 && p.Written == p.total()
}

func (p Part) isMaxRetry(attempt uint) bool {
	return attempt == p.maxTry
}

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

	ctx         context.Context
	name        string
	order       int
	maxTry      uint
	quiet       bool
	totalWriter io.Writer
	totalCancel func(bool)
	client      *http.Client
	dlogger     *log.Logger
}

type flashBar struct {
	*mpb.Bar
	prefix     string
	msgHandler func(*message)
}

func (b *flashBar) flash(msg string, final bool) {
	m := &message{
		times: 15,
		final: final,
		msg:   fmt.Sprintf("%s %s", b.prefix, msg),
	}
	b.msgHandler(m)
}

func (p Part) makeBar(progress *mpb.Progress, curTry *uint32) *flashBar {
	total := p.total()
	if total < 0 {
		total = 0
	}
	p.dlogger.Printf("Setting bar total: %d", total)
	msgCh := make(chan *message)
	b := progress.New(total,
		mpb.BarFillerBuilderFunc(func() mpb.BarFiller {
			if total == 0 {
				return mpb.NopStyle().Build()
			}
			return mpb.BarStyle().Lbound(" ").Rbound(" ").Build()
		}),
		mpb.BarFillerTrim(),
		mpb.BarPriority(p.order),
		mpb.PrependDecorators(
			newFlashDecorator(newMainDecorator(curTry, p.name, "%s %.1f", decor.WCSyncWidthR), msgCh),
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
			decor.AverageSpeed(decor.SizeB1024(0), "%.1f", decor.WCSyncSpace),
			decor.OnComplete(decor.Name("", decor.WCSyncSpace), "Peak:"),
			newSpeedPeak("%.1f", decor.WCSyncSpace),
		),
	)
	if p.Written > 0 {
		p.dlogger.Printf("Setting bar current: %d", p.Written)
		b.SetCurrent(p.Written)
		p.dlogger.Printf("Setting bar refill: %d", p.Written)
		b.SetRefill(p.Written)
	}
	if p.Elapsed > 0 {
		p.dlogger.Printf("Setting bar DecoratorAverageAdjust: -%d (-%[1]s)", p.Elapsed)
		b.DecoratorAverageAdjust(time.Now().Add(-p.Elapsed))
	}
	return &flashBar{
		Bar:        b,
		prefix:     p.name,
		msgHandler: makeMsgHandler(p.ctx, p.quiet, msgCh),
	}
}

func (p *Part) download(progress *mpb.Progress, req *http.Request, timeout, sleep time.Duration) (err error) {
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

	var bar *flashBar
	var curTry uint32
	var statusPartialContent bool
	resetTimeout := timeout
	prefix := p.dlogger.Prefix()
	barInitDone := make(chan struct{})

	return backoff.RetryWithContext(p.ctx, exponential.New(exponential.WithBaseDelay(500*time.Millisecond)),
		func(attempt uint, reset func()) (retry bool, err error) {
			atomic.StoreUint32(&curTry, uint32(attempt))
			var totalSleep time.Duration
			pWritten := p.Written
			start := time.Now()
			defer func() {
				if p.Skip {
					return
				}
				if n := p.Written - pWritten; n != 0 {
					reset()
					timeout = resetTimeout
					p.Elapsed += time.Since(start) - totalSleep
					p.dlogger.Printf("Total sleep: %s", totalSleep)
					p.dlogger.Printf("Written: %d", n)
				} else if timeout < maxTimeout*time.Second {
					timeout += 5 * time.Second
				}
				if retry && err != nil {
					switch attempt {
					case 0:
						atomic.StoreUint32(&globTry, 1)
					case p.maxTry:
						bar.flash(ErrMaxRetry.Error(), true)
						bar.Abort(false)
						retry, err = false, errors.Wrap(ErrMaxRetry, err.Error())
					}
					p.dlogger.Printf("Error: %s", err.Error())
				}
			}()

			req.Header.Set(hRange, p.getRange())

			p.dlogger.SetPrefix(fmt.Sprintf("%s[R%02d] ", prefix, attempt))
			p.dlogger.Printf("GET %q", req.URL)
			for k, v := range req.Header {
				p.dlogger.Printf("%s: %v", k, v)
			}

			ctx, cancel := context.WithCancel(p.ctx)
			defer cancel()
			timer := time.AfterFunc(timeout, func() {
				cancel()
				// checking for bar != nil here is a data race
				select {
				case <-barInitDone:
					bar.flash("Timeout...", false)
				default:
				}
				p.dlogger.Println("Timer expired")
			})
			defer timer.Stop()

			resp, err := p.client.Do(req.WithContext(ctx))
			if err != nil {
				if bar == nil {
					bar = p.makeBar(progress, &curTry)
					close(barInitDone)
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
			case http.StatusPartialContent:
				statusPartialContent = true
			case http.StatusOK: // no partial content, download with single part
				if statusPartialContent {
					return false, errors.New("http.StatusOK after http.StatusPartialContent")
				}
				if p.order != 1 {
					p.Skip = true
					p.dlogger.Println("Skip: no partial content")
					return false, nil
				}
				p.totalCancel(true) // single bar doesn't need total bar
				if resp.ContentLength > 0 {
					p.Stop = resp.ContentLength - 1
				}
				defer func() {
					if retry && err != nil {
						e := fpart.Close()
						if e != nil {
							panic(e)
						}
						fpart, e = os.OpenFile(p.FileName, os.O_WRONLY|os.O_TRUNC, 0644)
						if e != nil {
							panic(e)
						}
						p.Written = 0
						bar.SetCurrent(0)
					}
				}()
			default:
				if bar != nil {
					bar.flash(resp.Status, true)
				}
				return false, HttpError{resp.StatusCode, resp.Status}
			}

			if bar == nil {
				bar = p.makeBar(progress, &curTry)
				close(barInitDone)
			} else if p.Written > 0 {
				p.dlogger.Printf("Setting bar refill: %d", p.Written)
				bar.SetRefill(p.Written)
			}

			body := bar.ProxyReader(resp.Body)
			defer body.Close()

			buf := make([]byte, bufSize)
			dst := io.MultiWriter(fpart, p.totalWriter)
			sleepCtx, sleepCancel := context.WithCancel(p.ctx)
			sleepCancel()
			for n := 0; err == nil; sleepCancel() {
				timer.Reset(timeout)
				n, err = io.ReadFull(body, buf)
				switch err {
				case io.EOF:
					if n != 0 {
						panic("expected no bytes were read at EOF")
					}
					continue
				case io.ErrUnexpectedEOF:
					if n != 0 {
						err = nil
					} else {
						continue
					}
				default:
					if timer.Stop() && sleep != 0 {
						sleepCtx, sleepCancel = context.WithTimeout(p.ctx, sleep)
						totalSleep += sleep
					}
				}
				_, e := dst.Write(buf[:n])
				if e != nil {
					panic(e)
				}
				p.Written += int64(n)
				if p.total() <= 0 {
					bar.SetTotal(p.Written, false)
				}
				<-sleepCtx.Done()
			}

			if err == io.EOF {
				if p.total() <= 0 {
					p.Stop = p.Written - 1 // so p.isDone() retruns true
					bar.EnableTriggerComplete()
				}
				if p.isDone() {
					p.dlogger.Println("Part is done")
					return false, nil
				} else {
					panic("expected part to be done after EOF")
				}
			}

			if p.isDone() {
				panic(fmt.Sprintf("expected EOF after part is done, got: %v", err))
			}

			return p.ctx.Err() == nil, err
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
	return p.Written != 0 && p.Written == p.total()
}

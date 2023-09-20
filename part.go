package getparty

import (
	"context"
	"fmt"
	"io"
	"log"
	"net/http"
	"os"
	"strings"
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
	progress    *mpb.Progress
	dlogger     *log.Logger
}

type flashBar struct {
	*mpb.Bar
	msgHandler  func(message)
	prefix      string
	initialized uint32
}

func (b flashBar) flash(msg string, final bool) {
	msg = fmt.Sprintf("%s %s", b.prefix, msg)
	b.msgHandler(message{msg, final})
}

func (b flashBar) flashErr(msg string, final bool) {
	msg = fmt.Sprintf("ERR %s", msg)
	b.msgHandler(message{msg, final})
}

func (p Part) initBar(fb *flashBar, curTry *uint32) error {
	var barBuilder mpb.BarFillerBuilder
	msgCh := make(chan message)
	total := p.total()
	if total < 0 {
		total = 0
		barBuilder = mpb.NopStyle()
	} else {
		p.dlogger.Printf("Setting bar total: %d", total)
		barBuilder = mpb.BarStyle().Lbound(" ").Rbound(" ")
	}
	ctx, cancel := context.WithCancel(p.ctx)
	b, err := p.progress.Add(total, barBuilder.Build(), mpb.BarFillerTrim(), mpb.BarPriority(p.order),
		mpb.PrependDecorators(
			newFlashDecorator(
				newMainDecorator(curTry, p.name, "%s %.1f", decor.WCSyncWidthR),
				msgCh,
				cancel,
				16,
			),
			decor.Conditional(
				total == 0,
				decor.OnComplete(decor.Spinner([]string{`-`, `\`, `|`, `/`}, decor.WCSyncSpace), "100% "),
				decor.OnComplete(decor.NewPercentage("%.2f", decor.WCSyncSpace), "100%"),
			),
		),
		mpb.AppendDecorators(
			decor.OnCompleteOrOnAbort(
				decor.Conditional(
					total == 0,
					decor.Name(""),
					decor.NewAverageETA(
						decor.ET_STYLE_MMSS,
						time.Now(),
						decor.FixedIntervalTimeNormalizer(30),
						decor.WCSyncWidth,
					)), "Avg:"),
			decor.AverageSpeed(decor.SizeB1024(0), "%.1f", decor.WCSyncSpace),
			decor.OnCompleteOrOnAbort(decor.Name("", decor.WCSyncSpace), "Peak:"),
			newSpeedPeak("%.1f", decor.WCSyncSpace),
		),
	)
	if err != nil {
		return err
	}
	if p.Written != 0 {
		p.dlogger.Printf("Setting bar current: %d", p.Written)
		b.SetCurrent(p.Written)
		p.dlogger.Printf("Setting bar DecoratorAverageAdjust: (now - %s)", p.Elapsed.Truncate(time.Second))
		b.DecoratorAverageAdjust(time.Now().Add(-p.Elapsed))
	}
	fb.Bar = b
	fb.prefix = p.name
	fb.msgHandler = makeMsgHandler(ctx, msgCh, p.quiet)
	atomic.StoreUint32(&fb.initialized, 1)
	return nil
}

func (p *Part) download(client *http.Client, req *http.Request, timeout, sleep time.Duration) (err error) {
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

	var bar flashBar
	var curTry uint32
	var statusPartialContent bool
	resetTimeout := timeout
	prefix := p.dlogger.Prefix()

	return backoff.RetryWithContext(p.ctx, exponential.New(exponential.WithBaseDelay(500*time.Millisecond)),
		func(attempt uint, reset func()) (retry bool, err error) {
			atomic.StoreUint32(&curTry, uint32(attempt))
			var totalSleep time.Duration
			pWritten := p.Written
			start := time.Now()
			defer func() {
				p.dlogger.Printf("Retry: %v, Error: %v", retry, err)
				if p.Skip {
					return
				}
				if n := p.Written - pWritten; n != 0 {
					if n >= bufSize {
						reset()
						timeout = resetTimeout
					}
					p.Elapsed += time.Since(start) - totalSleep
					p.dlogger.Printf("Written: %d", n)
					p.dlogger.Printf("Total sleep: %s", totalSleep)
				} else if timeout < maxTimeout*time.Second {
					timeout += 5 * time.Second
				}
				if retry && err != nil {
					switch attempt {
					case 0:
						atomic.StoreUint32(&globTry, 1)
					case p.maxTry:
						if atomic.LoadUint32(&bar.initialized) == 1 {
							bar.flashErr(ErrMaxRetry.Error(), true)
							go bar.Abort(false)
						}
						retry, err = false, errors.Wrap(ErrMaxRetry, err.Error())
					}
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
				msg := "Timeout..."
				p.dlogger.Println(msg)
				// following check is needed, because this func runs in different goroutine
				// at first attempt bar may be not initialized by the time this func runs
				if atomic.LoadUint32(&bar.initialized) == 1 {
					bar.flash(msg, false)
				}
			})
			defer timer.Stop()

			resp, err := client.Do(req.WithContext(ctx))
			if err != nil {
				if p.Written != 0 && atomic.LoadUint32(&bar.initialized) == 0 {
					if err := p.initBar(&bar, &curTry); err != nil {
						return false, err
					}
				}
				return true, err
			}

			p.dlogger.Printf("HTTP status: %s", resp.Status)
			p.dlogger.Printf("ContentLength: %d", resp.ContentLength)

			if jar := client.Jar; jar != nil {
				if cookies := jar.Cookies(req.URL); len(cookies) != 0 {
					p.dlogger.Println("CookieJar:")
					for _, cookie := range cookies {
						p.dlogger.Printf("  %q", cookie)
					}
				}
			}

			switch resp.StatusCode {
			case http.StatusPartialContent:
				if atomic.LoadUint32(&bar.initialized) == 0 {
					if err := p.initBar(&bar, &curTry); err != nil {
						return false, err
					}
				}
				if p.Written != 0 {
					p.dlogger.Printf("Setting bar refill: %d", p.Written)
					bar.SetRefill(p.Written)
				}
				statusPartialContent = true
			case http.StatusOK: // no partial content, download with single part
				switch atomic.LoadUint32(&bar.initialized) {
				case 0:
					if p.Written != 0 {
						panic(fmt.Sprintf("expected 0 bytes got %d", p.Written))
					}
					if p.order != 1 {
						p.Skip = true
						p.dlogger.Println("Stopping: no partial content")
						return false, nil
					}
					if resp.ContentLength > 0 {
						p.Stop = resp.ContentLength - 1
					}
					if err := p.initBar(&bar, &curTry); err != nil {
						return false, err
					}
				default:
					if statusPartialContent {
						panic("http.StatusOK after http.StatusPartialContent")
					}
					if p.Written != 0 {
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
				}
			case http.StatusServiceUnavailable:
				if atomic.LoadUint32(&bar.initialized) == 1 {
					bar.flashErr(cutCode(resp.Status), false)
				} else if p.Written != 0 {
					if err := p.initBar(&bar, &curTry); err != nil {
						return false, err
					}
				}
				return true, HttpError{resp.StatusCode, resp.Status}
			default:
				if atomic.LoadUint32(&bar.initialized) == 1 {
					bar.flashErr(cutCode(resp.Status), true)
					go bar.Abort(false)
				}
				return false, HttpError{resp.StatusCode, resp.Status}
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

func cutCode(err string) string {
	_, remainder, found := strings.Cut(err, " ")
	if found {
		return remainder
	}
	return err
}

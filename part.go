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

	ctx          context.Context
	name         string
	order        int
	maxTry       uint
	quiet        bool
	single       bool
	progress     *mpb.Progress
	dlogger      *log.Logger
	totalEwmaInc func(int, time.Duration)
}

type flashBar struct {
	*mpb.Bar
	msgHandler  func(message)
	prefix      string
	initialized atomic.Bool
}

func (b *flashBar) flash(msg string) {
	msg = fmt.Sprintf("%s %s", b.prefix, msg)
	b.msgHandler(message{
		msg:   msg,
		error: false,
	})
}

func (b *flashBar) flashErr(msg string) {
	msg = fmt.Sprintf("%s:ERR %s", b.prefix, msg)
	b.msgHandler(message{
		msg:   msg,
		error: true,
	})
}

func (b *flashBar) init(p *Part, curTry *uint32) error {
	if b.initialized.Load() {
		return nil
	}
	var barBuilder mpb.BarFillerBuilder
	msgCh := make(chan message)
	total := p.total()
	if total < 0 {
		total = 0
		barBuilder = mpb.NopStyle()
	} else {
		barBuilder = mpb.BarStyle().Lbound(" ").Rbound(" ")
	}
	p.dlogger.Printf("Setting bar total: %d", total)
	bar, err := p.progress.Add(total, barBuilder.Build(), mpb.BarFillerTrim(), mpb.BarPriority(p.order),
		mpb.BarOptional(
			mpb.BarExtender(mpb.BarFillerFunc(
				func(w io.Writer, _ decor.Statistics) error {
					_, err := fmt.Fprintln(w)
					return err
				}), true),
			p.single),
		mpb.PrependDecorators(
			newFlashDecorator(newMainDecorator(curTry, p.name, "%s %.1f", decor.WCSyncWidthR), msgCh, 15),
			decor.Conditional(total == 0,
				decor.OnComplete(decor.Spinner([]string{`-`, `\`, `|`, `/`}, decor.WC{C: decor.DextraSpace}), "100% "),
				decor.OnComplete(decor.NewPercentage("%.2f", decor.WCSyncSpace), "100%")),
		),
		mpb.AppendDecorators(
			decor.Conditional(total == 0,
				decor.Name(""),
				decor.OnComplete(decor.NewAverageETA(
					decor.ET_STYLE_MMSS,
					time.Now(),
					decor.FixedIntervalTimeNormalizer(30),
					decor.WCSyncWidth,
				), ":"),
			),
			decor.EwmaSpeed(decor.SizeB1024(0), "%.1f", 30, decor.WCSyncSpace),
			decor.OnComplete(decor.Name("", decor.WCSyncSpace), "Peak:"),
			newSpeedPeak("%.1f", decor.WCSyncSpace),
		),
	)
	if err != nil {
		return err
	}
	if p.Written != 0 {
		p.dlogger.Printf("Setting bar current: %d", p.Written)
		bar.SetCurrent(p.Written)
		bar.DecoratorAverageAdjust(time.Now().Add(-p.Elapsed))
	}
	b.Bar = bar
	b.prefix = p.name
	b.msgHandler = makeMsgHandler(msgCh, p.quiet)
	b.initialized.Store(true)
	return nil
}

func (p *Part) download(client *http.Client, req *http.Request, timeout, sleep time.Duration) (err error) {
	fpart, err := os.OpenFile(p.FileName, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0644)
	if err != nil {
		return errors.WithMessage(err, p.name)
	}
	defer func() {
		if e := fpart.Close(); e != nil {
			p.dlogger.Printf("Part close error: %s", e.Error())
		} else if p.Written == 0 {
			p.dlogger.Printf("Removing: %q", fpart.Name())
			if e = os.Remove(p.FileName); e != nil {
				p.dlogger.Printf("Part remove error: %s", e.Error())
			}
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
						atomic.AddUint32(&globTry, 1)
					case p.maxTry:
						atomic.AddUint32(&globTry, ^uint32(0))
						fmt.Fprintf(p.progress, "%s%s: %.1f / %.1f\n",
							p.dlogger.Prefix(),
							ErrMaxRetry.Error(),
							decor.SizeB1024(p.Written),
							decor.SizeB1024(p.total()))
						if bar.initialized.Load() {
							bar.Abort(true)
						}
						retry, err = false, errors.Wrap(ErrMaxRetry, err.Error())
					}
				}
			}()

			req.Header.Set(hRange, p.getRange())

			p.dlogger.SetPrefix(fmt.Sprintf(prefix, attempt))
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
				if bar.initialized.Load() {
					bar.flash(msg)
				}
			})
			defer timer.Stop()

			resp, err := client.Do(req.WithContext(ctx))
			if err != nil {
				if p.Written == 0 {
					go fmt.Fprintf(p.progress, "%s%s\n", p.dlogger.Prefix(), err.Error())
				} else {
					err := bar.init(p, &curTry)
					if err != nil {
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
				err := bar.init(p, &curTry)
				if err != nil {
					return false, err
				}
				if p.Written != 0 {
					go func() {
						p.dlogger.Printf("Setting bar refill: %d", p.Written)
						bar.SetRefill(p.Written)
					}()
				}
				statusPartialContent = true
			case http.StatusOK: // no partial content, download with single part
				if statusPartialContent {
					panic("http.StatusOK after http.StatusPartialContent")
				}
				if p.Written == 0 {
					if p.order != 1 {
						p.Skip = true
						p.dlogger.Println("Stopping: no partial content")
						return false, nil
					}
					p.single = true
					if resp.ContentLength > 0 {
						p.Stop = resp.ContentLength - 1
					}
					err := bar.init(p, &curTry)
					if err != nil {
						return false, err
					}
				} else if bar.initialized.Load() {
					err := fpart.Close()
					if err != nil {
						panic(err)
					}
					fpart, err = os.OpenFile(p.FileName, os.O_WRONLY|os.O_TRUNC, 0644)
					if err != nil {
						panic(err)
					}
					p.Written = 0
					bar.SetCurrent(0)
				} else {
					panic(fmt.Sprintf("expected 0 bytes got %d", p.Written))
				}
			case http.StatusServiceUnavailable:
				if bar.initialized.Load() {
					go bar.flashErr(resp.Status)
				} else {
					go fmt.Fprintf(p.progress, "%s%s\n", p.dlogger.Prefix(), resp.Status)
				}
				return true, HttpError(resp.StatusCode)
			default:
				if attempt != 0 {
					atomic.AddUint32(&globTry, ^uint32(0))
				}
				fmt.Fprintf(p.progress, "%s%s\n", p.dlogger.Prefix(), resp.Status)
				if bar.initialized.Load() {
					bar.Abort(true)
				}
				return false, HttpError(resp.StatusCode)
			}

			defer resp.Body.Close()

			buf := make([]byte, bufSize)
			sleepCtx, sleepCancel := context.WithCancel(context.Background())
			sleepCancel()
			for n := 0; err == nil; sleepCancel() {
				timer.Reset(timeout)
				start := time.Now()
				n, err = io.ReadFull(resp.Body, buf)
				dur := time.Since(start)
				p.totalEwmaInc(n, dur)
				bar.EwmaIncrBy(n, dur)
				switch err {
				case io.EOF:
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
				_, e := fpart.Write(buf[:n])
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
					panic("part isn't done after EOF")
				}
			}

			if p.isDone() {
				panic(fmt.Sprintf("part is done before EOF: %v", err))
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

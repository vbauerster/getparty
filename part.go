package getparty

import (
	"context"
	"fmt"
	"io"
	"io/fs"
	"log"
	"net/http"
	"net/http/httptrace"
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
	Start   int64
	Stop    int64
	Written int64
	Elapsed time.Duration

	ctx               context.Context
	order             int
	maxTry            uint
	progress          *mpb.Progress
	dlogger           *log.Logger
	incrTotalBar      func(int)
	patcher           func(*http.Request)
	cancel            func()
	partialOK         func()
	firstHttp200      chan int
	httpStatusOK      bool
	sessionOutputName string
	name              string
}

func (p Part) newBar(curTry *uint32, single bool, msgCh chan string) (*mpb.Bar, error) {
	var filler, extender mpb.BarFiller
	total := p.total()
	if total < 0 {
		total = 0
	} else {
		filler = distinctBarRefiller(baseBarStyle()).Build()
	}
	if single {
		extender = mpb.BarFillerFunc(func(w io.Writer, _ decor.Statistics) error {
			_, err := fmt.Fprintln(w)
			return err
		})
	}
	p.dlogger.Printf("Setting bar total: %d", total)
	bar, err := p.progress.Add(total, filler,
		mpb.BarFillerTrim(),
		mpb.BarPriority(p.order),
		mpb.BarExtender(extender, true),
		mpb.PrependDecorators(
			newFlashDecorator(newMainDecorator(curTry, p.name, "%s %.1f", decor.WCSyncWidthR), msgCh, 15),
			decor.Conditional(total == 0,
				decor.OnComplete(decor.Spinner([]string{`-`, `\`, `|`, `/`}, decor.WC{C: decor.DextraSpace}), "100% "),
				decor.OnComplete(decor.NewPercentage("%.2f", decor.WCSyncSpace), "100%")),
		),
		mpb.AppendDecorators(
			decor.Conditional(total == 0,
				decor.Name(""),
				decor.OnCompleteOrOnAbort(decor.NewAverageETA(
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
		return nil, err
	}
	if p.Written != 0 {
		p.dlogger.Printf("Setting bar current: %d", p.Written)
		bar.SetCurrent(p.Written)
		bar.DecoratorAverageAdjust(time.Now().Add(-p.Elapsed))
	}
	return bar, nil
}

func (p *Part) download(client *http.Client, location string, single bool, timeout, sleep time.Duration) (err error) {
	var fpart *os.File
	defer func() {
		if fpart != nil {
			if e := fpart.Close(); e != nil {
				err = firstErr(err, e)
			} else if p.Written == 0 {
				p.dlogger.Printf("%q is empty, removing", fpart.Name())
				err = firstErr(err, os.Remove(fpart.Name()))
			}
		}
		err = errors.WithMessage(err, p.name)
		p.cancel()
	}()

	req, err := http.NewRequest(http.MethodGet, location, nil)
	if err != nil {
		return err
	}
	if p.patcher != nil {
		p.patcher(req)
	}

	var bar *mpb.Bar
	var curTry uint32
	var statusPartialContent bool
	msgCh := make(chan string, 1)
	resetTimeout := timeout
	prefix := p.dlogger.Prefix()

	return backoff.RetryWithContext(p.ctx, exponential.New(exponential.WithBaseDelay(500*time.Millisecond)),
		func(attempt uint, reset func()) (retry bool, err error) {
			atomic.StoreUint32(&curTry, uint32(attempt))
			var totalSleep time.Duration
			pWritten := p.Written
			start := time.Now()
			defer func() {
				if !statusPartialContent {
					if fpart != nil {
						err = firstErr(err, fpart.Close())
						fpart = nil
					}
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
				if err != nil {
					switch {
					case attempt == 0:
						atomic.AddUint32(&globTry, 1)
					case attempt == p.maxTry:
						fmt.Fprintf(p.progress, "%s%s: %.1f / %.1f\n",
							p.dlogger.Prefix(),
							ErrMaxRetry.Error(),
							decor.SizeB1024(p.Written),
							decor.SizeB1024(p.total()))
						retry, err = false, errors.Wrap(ErrMaxRetry, err.Error())
						fallthrough
					case !retry:
						atomic.AddUint32(&globTry, ^uint32(0))
						if bar != nil {
							bar.Abort(true)
						}
					}
				}
				p.dlogger.Printf("Retry: %v, Error: %v", retry, err)
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
				select {
				case msgCh <- fmt.Sprintf("%s %s", p.name, msg):
				case <-msgCh:
					p.dlogger.Println("Houston msg dropped, is bar initialized?")
				}
			})
			defer timer.Stop()

			trace := &httptrace.ClientTrace{
				GotConn: func(connInfo httptrace.GotConnInfo) {
					p.dlogger.Printf("Connection RemoteAddr: %s", connInfo.Conn.RemoteAddr())
				},
			}

			resp, err := client.Do(req.WithContext(httptrace.WithClientTrace(ctx, trace)))
			if err != nil {
				fmt.Fprintf(p.progress, "%s%s\n", p.dlogger.Prefix(), unwrapOrErr(err).Error())
				return true, err
			}
			defer func() {
				if resp.Body != nil {
					err = firstErr(err, resp.Body.Close())
				}
			}()

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

			for k, v := range resp.Header {
				p.dlogger.Printf("Response Header: %s: %v", k, v)
			}

			switch resp.StatusCode {
			case http.StatusPartialContent:
				p.partialOK()
				statusPartialContent = true
				if fpart == nil {
					fpart, err = os.OpenFile(p.outputName(), os.O_WRONLY|os.O_CREATE|os.O_APPEND, umask)
					if err != nil {
						return false, err
					}
				}
				if bar == nil {
					bar, err = p.newBar(&curTry, single, msgCh)
					if err != nil {
						return false, err
					}
				}
			case http.StatusOK: // no partial content, download with single part
				if statusPartialContent {
					panic("http.StatusOK after http.StatusPartialContent")
				}
				select {
				case p.firstHttp200 <- p.order:
					p.firstHttp200 = nil
					p.httpStatusOK = true
					if resp.ContentLength > 0 {
						p.Stop = resp.ContentLength - 1
					}
					if p.Written != 0 {
						panic(fmt.Sprintf("expected to start with written=0 got %d", p.Written))
					}
				default:
					if !p.httpStatusOK {
						p.dlogger.Println("Stopping: some other part got status 200")
						return false, nil
					}
				}
				fpart, err = os.OpenFile(p.sessionOutputName, os.O_WRONLY|os.O_CREATE|os.O_TRUNC, umask)
				if err != nil {
					return false, err
				}
				if bar == nil {
					bar, err = p.newBar(&curTry, true, msgCh)
					if err != nil {
						return false, err
					}
				} else {
					bar.SetCurrent(0)
					p.Written = 0
				}
			case http.StatusInternalServerError, http.StatusNotImplemented, http.StatusBadGateway, http.StatusServiceUnavailable, http.StatusGatewayTimeout:
				fmt.Fprintf(p.progress, "%s%s\n", p.dlogger.Prefix(), resp.Status)
				return true, HttpError(resp.StatusCode)
			default:
				fmt.Fprintf(p.progress, "%s%s\n", p.dlogger.Prefix(), resp.Status)
				return false, HttpError(resp.StatusCode)
			}

			if bar == nil {
				panic("expected non nil bar here")
			}
			if p.Written != 0 {
				go bar.SetRefill(p.Written)
			}

			buf := make([]byte, bufSize)
			sleepCtx, sleepCancel := context.WithCancel(context.Background())
			sleepCancel()
			for n := 0; err == nil; sleepCancel() {
				timer.Reset(timeout)
				start := time.Now()
				n, err = io.ReadFull(resp.Body, buf)
				dur := time.Since(start) + sleep
				if timer.Stop() && sleep != 0 {
					sleepCtx, sleepCancel = context.WithTimeout(p.ctx, sleep)
					totalSleep += sleep
				}
				if err == io.EOF || err == io.ErrUnexpectedEOF {
					if n == 0 {
						continue
					}
					// err is io.ErrUnexpectedEOF here
					// reset in order to have io.ReadFull return io.EOF
					err = nil
				}
				n, e := fpart.Write(buf[:n])
				if e != nil {
					err = e
					continue
				}
				p.Written += int64(n)
				bar.EwmaIncrBy(n, dur)
				if p.total() <= 0 {
					bar.SetTotal(p.Written, false)
				} else {
					p.incrTotalBar(n)
				}
				<-sleepCtx.Done()
			}

			if err == io.EOF {
				if p.total() <= 0 {
					p.Stop = p.Written - 1 // so p.isDone() retruns true
					bar.EnableTriggerComplete()
				}
				if p.isDone() {
					return false, fpart.Sync()
				}
				return false, errors.WithMessage(err, "Part isn't done after EOF")
			}

			if p.isDone() {
				message := "Part is done before EOF"
				err = errors.WithMessage(err, message)
				if err != nil {
					return false, err
				}
				return false, errors.New(message)
			}

			// true is ignored if err == nil
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
	return p.Written != 0 && p.Written == p.total()
}

func (p Part) checkSize(stat fs.FileInfo) error {
	size := stat.Size()
	if size != p.Written {
		return errors.Errorf("%q size mismatch: expected %d got %d", stat.Name(), p.Written, size)
	}
	return nil
}

func (p Part) outputName() string {
	if p.order == 0 {
		panic("part is not initialized")
	}
	return fmt.Sprintf("%s.%02d", p.sessionOutputName, p.order)
}

func (p Part) writeTo(dst *os.File) (err error) {
	src, err := os.Open(p.outputName())
	if err != nil {
		return err
	}
	defer func() {
		err = firstErr(err, src.Close())
		if err != nil {
			return
		}
		p.dlogger.Printf("Removing: %q", src.Name())
		err = os.Remove(src.Name())
	}()
	stat, err := src.Stat()
	if err != nil {
		return err
	}
	err = p.checkSize(stat)
	if err != nil {
		return err
	}
	n, err := io.Copy(dst, src)
	p.dlogger.Printf("%d bytes copied: src=%q dst=%q", n, src.Name(), dst.Name())
	return err
}

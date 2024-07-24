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

type http200Context struct {
	ctx    context.Context
	cancel func()
	first  chan int
}

// Part represents state of each download part
type Part struct {
	Start   int64
	Stop    int64
	Written int64
	Elapsed time.Duration

	ctx          context.Context
	client       *http.Client
	progress     *mpb.Progress
	logger       *log.Logger
	statusOK     *http200Context
	incrTotalBar func(int)
	patcher      func(*http.Request)
	cancel       func()
	name         string
	order        int
	debugWriter  io.Writer
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
	p.logger.Printf("Setting bar total: %d", total)
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
		p.logger.Printf("Setting bar current: %d", p.Written)
		bar.SetCurrent(p.Written)
		bar.DecoratorAverageAdjust(time.Now().Add(-p.Elapsed))
	}
	return bar, nil
}

func (p *Part) download(
	location, baseName string,
	timeout, sleep time.Duration,
	maxTry uint,
	single bool,
) (err error) {
	var fpart *os.File
	prefix := fmt.Sprintf("[%s:R%%02d] ", p.name)
	p.logger = log.New(p.debugWriter, fmt.Sprintf(prefix, 0), log.LstdFlags)
	defer func() {
		if fpart != nil {
			if e := fpart.Close(); e != nil {
				err = firstErr(err, e)
			} else if p.Written == 0 {
				p.logger.Printf("%q is empty, removing", fpart.Name())
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
	var httpStatus206 bool
	var httpStatus200 bool
	msgCh := make(chan string, 1)
	resetTimeout := timeout

	return backoff.RetryWithContext(p.ctx, exponential.New(exponential.WithBaseDelay(500*time.Millisecond)),
		func(attempt uint, reset func()) (retry bool, err error) {
			atomic.StoreUint32(&curTry, uint32(attempt))
			var totalSleep time.Duration
			pWritten := p.Written
			start := time.Now()
			defer func() {
				p.logger.Printf("Retry: %t, Error: %v", retry, err)
				if !httpStatus206 {
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
					p.logger.Printf("Written: %d", n)
					p.logger.Printf("Total sleep: %s", totalSleep)
				} else if timeout < maxTimeout*time.Second {
					timeout += 5 * time.Second
				}
				if err != nil {
					switch {
					case attempt == 0:
						atomic.AddUint32(&globTry, 1)
					case attempt == maxTry:
						fmt.Fprintf(p.progress, "%s%s: %.1f / %.1f\n",
							prefix,
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
			}()

			if attempt != 0 {
				prefix = fmt.Sprintf(prefix, attempt)
				p.logger.SetPrefix(prefix)
			}

			p.logger.Printf("GET %q", req.URL)

			req.Header.Set(hRange, p.getRange())
			for k, v := range req.Header {
				p.logger.Printf("Request Header: %s: %v", k, v)
			}

			ctx, cancel := context.WithCancel(p.ctx)
			defer cancel()
			timer := time.AfterFunc(timeout, func() {
				cancel()
				msg := "Timeout..."
				p.logger.Println(msg)
				select {
				case msgCh <- fmt.Sprintf("%s %s", p.name, msg):
				case <-msgCh:
					p.logger.Println("Houston msg dropped, is bar initialized?")
				}
			})
			defer timer.Stop()

			trace := &httptrace.ClientTrace{
				GotConn: func(connInfo httptrace.GotConnInfo) {
					p.logger.Printf("Connection RemoteAddr: %s", connInfo.Conn.RemoteAddr())
				},
			}

			resp, err := p.client.Do(req.WithContext(httptrace.WithClientTrace(ctx, trace)))
			if err != nil {
				fmt.Fprintf(p.progress, "%s%s\n", prefix, unwrapOrErr(err).Error())
				return true, err
			}
			defer func() {
				if resp.Body != nil {
					err = firstErr(err, resp.Body.Close())
				}
			}()

			p.logger.Printf("HTTP status: %s", resp.Status)

			if jar := p.client.Jar; jar != nil {
				for _, cookie := range jar.Cookies(req.URL) {
					p.logger.Printf("Cookie: %s", cookie) // cookie implements fmt.Stringer
				}
			}

			for k, v := range resp.Header {
				p.logger.Printf("Response Header: %s: %v", k, v)
			}

			switch resp.StatusCode {
			case http.StatusPartialContent:
				p.statusOK.cancel()
				httpStatus206 = true
				if fpart == nil {
					fpart, err = os.OpenFile(p.outputName(baseName), os.O_WRONLY|os.O_CREATE|os.O_APPEND, umask)
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
				if p.Written != 0 {
					go bar.SetRefill(p.Written)
				}
			case http.StatusOK: // no partial content, download with single part
				if httpStatus206 {
					panic("http.StatusOK after http.StatusPartialContent")
				}
				select {
				case p.statusOK.first <- p.order:
					p.statusOK.cancel()
					httpStatus200 = true
					if resp.ContentLength > 0 {
						p.Stop = resp.ContentLength - 1
					}
					if p.Written != 0 {
						panic(fmt.Sprintf("expected to start with written=0 got %d", p.Written))
					}
				case <-p.statusOK.ctx.Done():
					if !httpStatus200 {
						p.logger.Println("Stopping: some other part got status 200")
						return false, nil
					}
				}
				fpart, err = os.OpenFile(baseName, os.O_WRONLY|os.O_CREATE|os.O_TRUNC, umask)
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
				fmt.Fprintf(p.progress, "%s%s\n", prefix, resp.Status)
				return true, HttpError(resp.StatusCode)
			default:
				fmt.Fprintf(p.progress, "%s%s\n", prefix, resp.Status)
				return false, HttpError(resp.StatusCode)
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

func (p Part) outputName(base string) string {
	if p.order == 0 {
		panic("part is not initialized")
	}
	return fmt.Sprintf("%s.%02d", base, p.order)
}

func (p Part) writeTo(dst *os.File) (err error) {
	src, err := os.Open(p.outputName(dst.Name()))
	if err != nil {
		return err
	}
	defer func() {
		err = firstErr(err, src.Close())
		if err != nil {
			return
		}
		p.logger.Printf("Removing: %q", src.Name())
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
	p.logger.Printf("%d bytes copied: src=%q dst=%q", n, src.Name(), dst.Name())
	return err
}

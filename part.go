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

const bufMax = 1 << 14

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

	ctx            context.Context
	client         *http.Client
	progress       *mpb.Progress
	logger         *log.Logger
	statusOK       *http200Context
	patcher        func(*http.Request)
	totalIncr      chan<- int
	order          int
	single         bool
	name           string
	prefixTemplate string
}

func (p Part) newBar(curTry *uint32, msgCh <-chan string) (*mpb.Bar, error) {
	var filler mpb.BarFiller
	total := p.total()
	if total < 0 {
		p.logger.Println("Session must be not resumable")
		total = 0
	} else {
		filler = distinctBarRefiller(baseBarStyle()).Build()
	}
	p.logger.Println("Setting bar total:", total)
	bar, err := p.progress.Add(total, filler,
		mpb.BarFillerTrim(),
		mpb.BarPriority(p.order),
		mpb.PrependDecorators(
			newFlashDecorator(newMainDecorator(curTry, p.name, "%s %.1f", decor.WCSyncWidthR), msgCh, 15),
			decor.Conditional(total == 0,
				decor.OnComplete(decor.Spinner([]string{`-`, `\`, `|`, `/`}, decor.WC{C: decor.DextraSpace}), "100% "),
				decor.OnComplete(decor.NewPercentage("%.2f", decor.WCSyncSpace), "100%")),
		),
		mpb.AppendDecorators(
			decor.Conditional(total == 0,
				decor.Name(""),
				decor.OnCompleteOrOnAbort(decor.EwmaNormalizedETA(
					decor.ET_STYLE_MMSS,
					30,
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
		p.logger.Println("Setting bar current:", p.Written)
		bar.SetCurrent(p.Written)
	}
	return bar, nil
}

func (p *Part) initDebugLogger(out io.Writer, prefixTemplate string) {
	p.logger = log.New(out, fmt.Sprintf(prefixTemplate, 0), log.LstdFlags)
	p.prefixTemplate = prefixTemplate
}

func (p *Part) download(
	location, outputBase string,
	bufSize, maxTry uint,
	sleep, timeout time.Duration,
) (err error) {
	var fpart *os.File
	var totalElapsed, totalSlept time.Duration
	defer func() {
		p.logger.Println("Total Written:", p.Written)
		p.logger.Println("Total Elapsed:", totalElapsed)
		p.logger.Println("Total Slept:", totalSlept)
		if fpart != nil {
			p.logger.Printf("Closing: %q", fpart.Name())
			err = firstErr(err, fpart.Close())
		}
		err = errors.WithMessage(err, p.name)
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
	msgCh := make(chan string, 1)
	resetTimeout := timeout

	bufLen := int(bufSize * 1024)
	p.logger.Println("ReadFull buf len:", bufLen)

	return backoff.RetryWithContext(p.ctx, exponential.New(exponential.WithBaseDelay(500*time.Millisecond)),
		func(attempt uint, reset func()) (retry bool, err error) {
			ctx, cancel := context.WithCancel(p.ctx)
			timer := time.AfterFunc(timeout, func() {
				cancel()
				msg := "Timeout..."
				select {
				case msgCh <- fmt.Sprintf("%s %s", p.name, msg):
					p.logger.Println(msg, "msg sent")
				case <-msgCh:
					p.logger.Println(msg, "msg dropped")
				}
			})
			var slept time.Duration
			pWritten := p.Written
			start := time.Now()
			defer func() {
				elapsed := time.Since(start)
				timer.Stop()
				cancel()
				totalElapsed += elapsed
				totalSlept += slept
				p.logger.Println("Written:", p.Written-pWritten)
				p.logger.Println("Elapsed:", elapsed)
				p.logger.Println("Slept:", slept)
				if !retry || err == nil || context.Cause(p.ctx) == ErrCanceledByUser {
					return
				}
				switch attempt {
				case 0:
					atomic.AddUint32(&globTry, 1)
				case maxTry:
					atomic.AddUint32(&globTry, ^uint32(0))
					retry, err = false, errors.WithMessage(ErrMaxRetry, "Stopping")
					fmt.Fprintf(p.progress, "%s%s (%.1f / %.1f)\n",
						p.logger.Prefix(),
						err.Error(),
						decor.SizeB1024(p.Written),
						decor.SizeB1024(p.total()))
					if bar != nil {
						bar.Abort(true)
					}
					return
				}
				go func(prefix string) {
					fmt.Fprintf(p.progress, "%s%s\n", prefix, unwrapOrErr(err).Error())
				}(p.logger.Prefix())
				p.logger.SetPrefix(fmt.Sprintf(p.prefixTemplate, attempt+1))
				atomic.StoreUint32(&curTry, uint32(attempt+1))
			}()

			p.logger.Printf("GET(%s): %s", timeout, req.URL)

			req.Header.Set(hRange, p.getRange())
			for k, v := range req.Header {
				p.logger.Printf("Request Header: %s: %v", k, v)
			}

			trace := &httptrace.ClientTrace{
				GotConn: func(connInfo httptrace.GotConnInfo) {
					p.logger.Println("Connection RemoteAddr:", connInfo.Conn.RemoteAddr())
				},
			}

			resp, err := p.client.Do(req.WithContext(httptrace.WithClientTrace(ctx, trace)))
			if err != nil {
				if !timer.Stop() && timeout < maxTimeout*time.Second {
					// timer has expired and f passed to time.AfterFunc has been started in its own goroutine
					timeout += 5 * time.Second
				}
				return true, err
			}
			defer func() {
				if resp.Body != nil {
					err = firstErr(err, resp.Body.Close())
				}
			}()

			p.logger.Println("HTTP response:", resp.Status)

			if jar := p.client.Jar; jar != nil {
				for _, cookie := range jar.Cookies(req.URL) {
					p.logger.Println("Cookie:", cookie) // cookie implements fmt.Stringer
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
					fpart, err = os.OpenFile(p.outputName(outputBase), os.O_WRONLY|os.O_CREATE|os.O_APPEND, umask)
					if err != nil {
						return false, err
					}
				}
				if bar == nil {
					bar, err = p.newBar(&curTry, msgCh)
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
					p.single = true
					if resp.ContentLength > 0 {
						p.Stop = resp.ContentLength - 1
					}
					if p.Written != 0 {
						panic(fmt.Sprintf("expected to start with written=0 got %d", p.Written))
					}
				case <-p.statusOK.ctx.Done():
					if !p.single {
						p.logger.Println("Stopping: some other part got status 200")
						return false, nil
					}
				}
				if fpart != nil {
					p.logger.Printf("Closing: %q", fpart.Name())
					err = firstErr(err, fpart.Close())
					if err != nil {
						return false, err
					}
				}
				fpart, err = os.OpenFile(outputBase, os.O_WRONLY|os.O_CREATE|os.O_TRUNC, umask)
				if err != nil {
					return false, err
				}
				if bar == nil {
					bar, err = p.newBar(&curTry, msgCh)
					if err != nil {
						return false, err
					}
				} else {
					bar.SetCurrent(0)
					p.Written = 0
				}
			case http.StatusInternalServerError, http.StatusNotImplemented, http.StatusBadGateway, http.StatusServiceUnavailable, http.StatusGatewayTimeout:
				return true, errors.Wrap(BadHttpStatus(resp.StatusCode), resp.Status)
			default:
				if attempt != 0 {
					atomic.AddUint32(&globTry, ^uint32(0))
				}
				fmt.Fprintf(p.progress, "%s%s\n", p.logger.Prefix(), BadHttpStatus(resp.StatusCode).Error())
				if bar != nil {
					bar.Abort(true)
				}
				return false, errors.Wrap(BadHttpStatus(resp.StatusCode), resp.Status)
			}

			var buf [bufMax]byte
			var sleepCtx context.Context
			sleepCancel := func() {}
			fuser := makeUnexpectedEOFFuser(p.logger)
			timer.Reset(timeout) // because client.Do has taken some time

			for n := bufLen; n == bufLen || fuser(err); sleepCancel() {
				start := time.Now()
				n, err = io.ReadFull(resp.Body, buf[:bufLen])
				rDur := time.Since(start)

				if timer.Reset(timeout + sleep) {
					// put off f passed to time.AfterFunc to be called for the next reset duration
					if sleep != 0 {
						sleepCtx, sleepCancel = context.WithTimeout(p.ctx, sleep)
						slept += sleep
					}
					if timeout != resetTimeout {
						reset()
						timeout = resetTimeout
					}
				} else if timeout < maxTimeout*time.Second {
					// timer has expired and f passed to time.AfterFunc has been started in its own goroutine
					// deferred timer.Stop will cancel former timer.Reset which scheduled f to run again
					// we're going to quit loop because n != bufLen most likely
					timeout += 5 * time.Second
				}

				wn, werr := fpart.Write(buf[:n])
				if werr != nil {
					sleepCancel()
					return false, errors.Wrap(werr, "Write error")
				}
				if wn != 0 {
					p.Written += int64(wn)
					if p.total() <= 0 {
						bar.SetTotal(p.Written, false)
					} else if !p.single {
						p.totalIncr <- wn
					}
				}
				bar.EwmaIncrBy(wn, rDur+sleep)
				if sleepCtx != nil {
					<-sleepCtx.Done()
				}
			}

			if p.total() <= 0 && err == io.EOF {
				// make sure next p.total() result is never negative
				p.Stop = p.Written - 1
				bar.EnableTriggerComplete()
			}

			if p.isDone() {
				p.logger.Println("Part is done")
				if err == io.EOF {
					return false, fpart.Sync()
				}
				return false, errors.Wrap(err, "Expected EOF")
			}

			// err is never nil here
			return true, err
		})
}

func (p Part) getRange() string {
	if p.Stop <= 0 {
		return "bytes=0-"
	}
	return fmt.Sprintf("bytes=%d-%d", p.Start+p.Written, p.Stop)
}

// on ContentLength =  0 p.Stop is -1 and total evaluates to  0
// on ContentLength = -1 p.Stop is -2 and total evaluates to -1
func (p Part) total() int64 {
	return p.Stop - p.Start + 1
}

func (p Part) isDone() bool {
	return p.Written == p.total()
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
	if p.single {
		return base
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

func makeUnexpectedEOFFuser(logger *log.Logger) func(error) bool {
	var fused bool
	return func(err error) bool {
		defer func() {
			fused = true
		}()
		logger.Printf("Fuser(%t): %v", fused, err)
		return err == io.ErrUnexpectedEOF && !fused
	}
}

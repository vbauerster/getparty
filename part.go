package getparty

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log"
	"net/http"
	"net/http/httptrace"
	"os"
	"sync/atomic"
	"time"

	"github.com/vbauerster/backoff"
	"github.com/vbauerster/backoff/exponential"
	"github.com/vbauerster/mpb/v8"
	"github.com/vbauerster/mpb/v8/decor"
)

const bufMax = 1 << 14

var globTry uint32

type httpStatusContext struct {
	ctx    context.Context
	cancel context.CancelCauseFunc
	ok     chan int
	quit   chan struct{}
}

// Part represents state of each download part
type Part struct {
	Start   int64
	Stop    int64
	Written int64

	id           int
	ctx          context.Context
	cancel       context.CancelFunc
	client       *http.Client
	file         *os.File
	progress     *progress
	logger       *log.Logger
	status       *httpStatusContext
	patcher      func(*http.Request)
	single       bool
	name         string
	base         string
	prefixFormat string
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
		mpb.BarPriority(p.id),
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
					60,
					decor.FixedIntervalTimeNormalizer(60),
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

func (p *Part) init(id int, session *Session, debug io.Writer) error {
	p.id = id
	p.base = session.OutputName
	p.single = session.Single
	p.name = fmt.Sprintf("P%02d", id)
	p.prefixFormat = fmt.Sprintf("[%s:R%%02d] ", p.name)
	p.logger = log.New(debug, fmt.Sprintf(p.prefixFormat, 0), log.LstdFlags)
	if session.restored && p.Written != 0 {
		stat, err := os.Stat(p.outputName())
		if err != nil {
			return withStack(err)
		}
		if size := stat.Size(); size != p.Written {
			err = fmt.Errorf("%q size mismatch: expected %d got %d", stat.Name(), p.Written, size)
		}
		return withStack(err)
	}
	return nil
}

func (p *Part) download(
	location string,
	bufSize, maxTry uint,
	sleep, timeout time.Duration,
) (err error) {
	var totalElapsed, totalIdle time.Duration
	defer func() {
		p.logger.Println("Total Written:", p.Written)
		p.logger.Println("Total Elapsed:", totalElapsed)
		p.logger.Println("Total Idle:", totalIdle)
		if p.cancel != nil {
			p.cancel()
		}
		err = withMessage(err, p.name)
	}()

	req, err := http.NewRequest(http.MethodGet, location, nil)
	if err != nil {
		return withStack(err)
	}
	if p.patcher != nil {
		p.patcher(req)
	}

	var bar *mpb.Bar
	var curTry uint32
	msgCh := make(chan string, 1)
	resetTimeout := timeout

	var buffer [bufMax]byte
	bufLen := int(bufSize * 1024)
	p.logger.Println("ReadFull buf len:", bufLen)

	trace := &httptrace.ClientTrace{
		GotConn: func(connInfo httptrace.GotConnInfo) {
			p.logger.Println("Connection RemoteAddr:", connInfo.Conn.RemoteAddr())
		},
	}

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
			var idle time.Duration
			start := time.Now()
			defer func(written int64) {
				timer.Stop()
				cancel()
				elapsed := time.Since(start)
				totalElapsed += elapsed
				totalIdle += idle
				p.logger.Println("Written:", p.Written-written)
				p.logger.Println("Elapsed:", elapsed)
				p.logger.Println("Idle:", idle)
				if !retry || err == nil || context.Cause(p.ctx) == ErrCanceledByUser {
					return
				}
				switch attempt {
				case 0:
					atomic.AddUint32(&globTry, 1)
				case maxTry:
					atomic.AddUint32(&globTry, ^uint32(0))
					retry, err = false, ErrMaxRetry
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
				p.logger.SetPrefix(fmt.Sprintf(p.prefixFormat, attempt+1))
				atomic.StoreUint32(&curTry, uint32(attempt+1))
			}(p.Written)

			p.logger.Printf("GET(%s): %s", timeout, req.URL)

			req.Header.Set(hRange, p.getRange())
			for k, v := range req.Header {
				p.logger.Printf("Request Header: %s: %v", k, v)
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
				if p.file == nil {
					p.status.cancel(errUnexpectedOK)
					p.file, err = os.OpenFile(p.outputName(), os.O_WRONLY|os.O_CREATE|os.O_APPEND, umask)
					if err != nil {
						return false, withStack(err)
					}
					bar, err = p.newBar(&curTry, msgCh)
					if err != nil {
						return false, withStack(err)
					}
				}
				if p.Written != 0 {
					go bar.SetRefill(p.Written)
				}
			case http.StatusOK: // no partial content, fallback to single part mode
				select {
				case p.status.ok <- p.id:
					p.single = true
					if resp.ContentLength > 0 {
						p.Stop = resp.ContentLength - 1
					}
					if p.file != nil || bar != nil {
						panic(fmt.Errorf("expected uninitialized got: p.file=%#v bar=%#v", p.file, bar))
					}
					p.file, err = os.OpenFile(p.outputName(), os.O_WRONLY|os.O_CREATE|os.O_TRUNC, umask)
					if err != nil {
						return false, withStack(err)
					}
					bar, err = p.newBar(&curTry, msgCh)
					if err != nil {
						return false, withStack(err)
					}
				case <-p.status.ctx.Done():
					if !p.single {
						p.single = true
						err := context.Cause(p.status.ctx)
						if err == errUnexpectedOK {
							panic(err)
						}
						// if either bar gets status ok, other bars shall quit silently
						p.logger.Printf("Quit: %v", err)
						return false, nil
					}
				}
				if p.Written != 0 {
					// on retry and status ok there is no way to resume so retry from scratch
					err := p.file.Truncate(0)
					if err != nil {
						return false, withStack(err)
					}
					p.Written = 0
					bar.SetCurrent(0)
				}
			case http.StatusInternalServerError, http.StatusNotImplemented, http.StatusBadGateway, http.StatusServiceUnavailable, http.StatusGatewayTimeout:
				return true, UnexpectedHttpStatus(resp.StatusCode)
			default:
				if attempt != 0 {
					atomic.AddUint32(&globTry, ^uint32(0))
				}
				err := withStack(UnexpectedHttpStatus(resp.StatusCode))
				fmt.Fprintf(p.progress, "%s%s\n", p.logger.Prefix(), err.Error())
				if bar != nil {
					bar.Abort(true)
				}
				return false, err
			}

			var resetOk bool
			var sleepCtx context.Context
			sleepCancel := func() {}
			fuser := makeUnexpectedEOFFuser(p.logger)
			timer.Reset(timeout) // because client.Do has taken some time

			for n := bufLen; n == bufLen || fuser(err); sleepCancel() {
				start := time.Now()
				n, err = io.ReadFull(resp.Body, buffer[:bufLen])
				rDur := time.Since(start)

				if timer.Reset(timeout + sleep) {
					// put off f passed to time.AfterFunc to be called for the next reset duration
					if sleep != 0 {
						sleepCtx, sleepCancel = context.WithTimeout(p.ctx, sleep)
						idle += sleep
					}
					if timeout != resetTimeout && resetOk {
						reset()
						timeout = resetTimeout
					}
					resetOk = true
				} else if timeout < maxTimeout*time.Second {
					// timer has expired and f passed to time.AfterFunc has been started in its own goroutine
					// deferred timer.Stop will cancel former timer.Reset which scheduled f to run again
					// we're going to quit loop because n != bufLen most likely
					timeout += 5 * time.Second
				}

				wn, werr := p.file.Write(buffer[:n])
				if werr != nil {
					sleepCancel()
					err := p.file.Truncate(p.Written)
					if err != nil {
						p.logger.Println("Truncate:", err.Error())
						return false, withStack(errors.Join(err, werr))
					}
					return false, withStack(werr)
				}
				p.Written += int64(wn)
				if p.total() <= 0 {
					if err == io.EOF {
						// make sure next p.total() result is never negative
						p.Stop = p.Written - 1
						bar.SetTotal(p.Written, true)
					} else {
						bar.SetTotal(p.Written, false)
					}
				} else if !p.single && wn != 0 {
					p.progress.total <- wn
				}
				bar.EwmaIncrBy(wn, rDur+sleep)
				if sleepCtx != nil {
					<-sleepCtx.Done()
				}
			}

			if p.isDone() {
				if err == io.EOF {
					p.logger.Println("Part is done")
					return false, withStack(p.file.Sync())
				}
				return false, withStack(fmt.Errorf("expected EOF, got: %w", err))
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

func (p Part) outputName() string {
	if p.id == 0 {
		panic(errors.New("part is not initialized"))
	}
	if p.single {
		return p.base
	}
	return fmt.Sprintf("%s.%02d", p.base, p.id)
}

func (p Part) writeTo(dst *os.File) (err error) {
	if p.file == nil {
		return withStack(fmt.Errorf("expected non nil file on %s", p.name))
	}
	// The behavior of Seek on a file opened with O_APPEND is not specified.
	// Have to reopen p.file which was initially opened with O_APPEND flag.
	err = firstErr(p.checkSize(), p.file.Close())
	if err != nil {
		return err
	}
	src, err := os.Open(p.file.Name())
	if err != nil {
		return withStack(err)
	}
	defer func() {
		err = firstErr(err, withStack(src.Close()))
		if err == nil {
			err = withStack(os.Remove(src.Name()))
			p.logger.Printf("%q removed with: %v", src.Name(), err)
		}
	}()
	n, err := io.Copy(dst, src)
	p.logger.Printf("%d bytes copied: dst=%q src=%q", n, dst.Name(), src.Name())
	return withStack(err)
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

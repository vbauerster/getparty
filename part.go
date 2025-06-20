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
const timeoutMsg = "Timeout..."

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
	output       string
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

func (p *Part) init(id int, session *Session) error {
	p.id = id
	p.single = session.Single
	p.name = fmt.Sprintf("P%02d", id)
	p.output = fmt.Sprintf("%s.%02d", session.OutputName, id)
	p.prefixFormat = fmt.Sprintf("[%s:R%%02d] ", p.name)
	if session.restored && p.Written != 0 {
		stat, err := os.Stat(p.output)
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

func (p *Part) download(location string, bufSize, maxTry uint, sleep, initialTimeout time.Duration) (err error) {
	var totalElapsed, totalIdle time.Duration
	defer func() {
		p.logger.Println("Total Written:", p.Written)
		p.logger.Println("Total Elapsed:", totalElapsed)
		p.logger.Println("Total Idle:", totalIdle)
		p.cancel()
		err = withMessage(err, p.name)
	}()

	p.logger = log.New(p.progress.err, fmt.Sprintf(p.prefixFormat, 0), log.LstdFlags)

	req, err := http.NewRequest(http.MethodGet, location, nil)
	if err != nil {
		return withStack(err)
	}
	if p.patcher != nil {
		p.patcher(req)
	}

	var bar *mpb.Bar
	var dta int // decrease timeout after
	var curTry uint32
	var buffer [bufMax]byte

	bufLen := int(min(bufMax, bufSize*1024))
	consecutiveResetOk := 32 / int(bufSize)
	timeout := initialTimeout
	barReady, barMsg := make(chan struct{}), make(chan string, 1)
	trace := &httptrace.ClientTrace{
		GotConn: func(connInfo httptrace.GotConnInfo) {
			p.logger.Println("Connection RemoteAddr:", connInfo.Conn.RemoteAddr())
		},
	}

	p.logger.Println("ReadFull buf len:", bufLen)

	return backoff.RetryWithContext(p.ctx, exponential.New(exponential.WithBaseDelay(500*time.Millisecond)),
		func(attempt uint, backoffReset func()) (retry bool, err error) {
			ctx, cancel := context.WithCancel(p.ctx)
			timer := time.AfterFunc(timeout, func() {
				cancel()
				select {
				case <-barReady:
					select {
					case barMsg <- fmt.Sprintf("%s %s", p.name, timeoutMsg):
						p.logger.Println(timeoutMsg, "msg sent")
					default:
					}
				default:
					p.logger.Println(timeoutMsg, "bar is not ready")
				}
			})
			var idle time.Duration
			start := time.Now()
			defer func(written int64) {
				if !timer.Stop() {
					if timeout < maxTimeout*time.Second {
						timeout += 5 * time.Second
					}
					dta += consecutiveResetOk
				}
				cancel()
				elapsed := time.Since(start)
				totalElapsed += elapsed
				totalIdle += idle
				p.logger.Println("Written:", p.Written-written)
				p.logger.Println("Elapsed:", elapsed)
				p.logger.Println("Idle:", idle)
				if !retry || err == nil || errors.Is(context.Cause(p.ctx), ErrCanceledByUser) {
					return
				}
				switch attempt {
				case 0:
					atomic.AddUint32(&globTry, 1)
				case maxTry:
					atomic.AddUint32(&globTry, ^uint32(0))
					retry, err = false, ErrMaxRetry
					_, _ = fmt.Fprintf(p.progress, "%s%s (%.1f / %.1f)\n",
						p.logger.Prefix(),
						err.Error(),
						decor.SizeB1024(p.Written),
						decor.SizeB1024(p.total()))
					if bar != nil {
						bar.Abort(!p.single)
					}
					return
				}
				go func(prefix string) {
					if errors.Is(ctx.Err(), context.Canceled) {
						prefix += timeoutMsg
					} else if prefix != "" {
						prefix = prefix[:len(prefix)-1]
					}
					_, _ = fmt.Fprintln(p.progress, prefix, unwrapOrErr(err).Error())
				}(p.logger.Prefix())
				p.logger.SetPrefix(fmt.Sprintf(p.prefixFormat, attempt+1))
				atomic.StoreUint32(&curTry, uint32(attempt+1))
			}(p.Written)

			p.logger.Printf("GET(%s,%d): %s", timeout, dta, req.URL)

			req.Header.Set(hRange, p.getRange())
			for k, v := range req.Header {
				p.logger.Printf("Request Header: %s: %v", k, v)
			}

			resp, err := p.client.Do(req.WithContext(httptrace.WithClientTrace(ctx, trace)))
			if err != nil {
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
					p.status.cancel(nil)
					p.file, err = os.OpenFile(p.output, os.O_WRONLY|os.O_CREATE|os.O_APPEND, umask)
					if err != nil {
						return false, withStack(err)
					}
					bar, err = p.newBar(&curTry, barMsg)
					if err != nil {
						return false, withStack(err)
					}
					close(barReady)
				} else if p.single {
					err := context.Cause(p.status.ctx)
					if !errors.Is(err, context.Canceled) {
						// StatusPartialContent after StatusOK
						panic(UnexpectedHttpStatus(http.StatusPartialContent))
					}
				}
				if p.Written != 0 {
					go bar.SetRefill(p.Written)
				}
			case http.StatusOK: // no partial content, fallback to single part mode
				select {
				case p.status.ok <- p.id:
					p.status.cancel(singleModeFallback(p.id))
					p.single = true
					if resp.ContentLength > 0 {
						p.Stop = resp.ContentLength - 1
					}
					if p.file != nil {
						panic(fmt.Errorf("expected nil %T, got %#[1]v", p.file))
					}
					p.file, err = os.OpenFile(p.output, os.O_WRONLY|os.O_CREATE|os.O_TRUNC, umask)
					if err != nil {
						return false, withStack(err)
					}
					if bar != nil {
						panic(fmt.Errorf("expected nil %T, got %#[1]v", bar))
					}
					bar, err = p.newBar(&curTry, barMsg)
					if err != nil {
						return false, withStack(err)
					}
					close(barReady)
				case <-p.status.ctx.Done():
					err := context.Cause(p.status.ctx)
					if errors.Is(err, context.Canceled) {
						// StatusOK after StatusPartialContent
						panic(UnexpectedHttpStatus(http.StatusOK))
					}
					if !p.single {
						p.single = true
						// if either bar gets status ok, other bars shall quit silently
						p.logger.Printf("Quit: %s", err.Error())
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
				_, _ = fmt.Fprintf(p.progress, "%s%s\n", p.logger.Prefix(), err.Error())
				if bar != nil {
					bar.Abort(!p.single)
				}
				return false, err
			}

			expired := !timer.Reset(timeout + sleep)
			reset := func() {
				if expired {
					p.logger.Println("timer.Reset had expired")
					return
				}
				if timeout > initialTimeout {
					switch dta {
					case 0:
						timeout -= 5 * time.Second
						dta = consecutiveResetOk
						if timeout == initialTimeout {
							backoffReset()
						}
					default:
						dta--
					}
				}
				if sleep != 0 {
					timer := time.NewTimer(sleep)
					defer timer.Stop()
					select {
					case <-timer.C:
						idle += sleep
					case <-ctx.Done():
						return
					}
				}
				expired = !timer.Reset(timeout + sleep)
			}
			fuser := makeUnexpectedEOFFuser(p.logger)

			for n := bufLen; n == bufLen || fuser(err); reset() {
				start := time.Now()
				n, err = io.ReadFull(resp.Body, buffer[:bufLen])
				rDur := time.Since(start)

				wn, werr := p.file.Write(buffer[:n])
				if werr != nil {
					err := p.file.Truncate(p.Written)
					if err != nil {
						return false, withStack(errors.Join(err, werr))
					}
					return false, withStack(werr)
				}

				p.Written += int64(wn)

				switch {
				case p.total() <= 0:
					bar.SetTotal(p.Written, false)
					if err == io.EOF {
						// make sure next p.total() result is never negative
						p.Stop = p.Written - 1
						bar.EnableTriggerComplete()
					}
				case !p.single && wn != 0:
					p.progress.total <- wn
				}
				bar.EwmaIncrBy(wn, rDur+sleep)
			}

			if p.isDone() {
				if err == io.EOF {
					p.logger.Println("Part is done")
					return false, nil
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

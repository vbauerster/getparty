package getparty

import (
	"cmp"
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
	client       *http.Client        // shared among parts
	progress     *progress           // shared among parts
	status       *httpStatusContext  // shared among parts
	patcher      func(*http.Request) // shared among parts
	logger       *log.Logger
	file         *os.File
	name         string
	output       string
	prefixFormat string
	single       bool
}

func (p Part) newBar(curTry *uint32, msgCh <-chan string) (*mpb.Bar, error) {
	var filler mpb.BarFiller
	total := p.total()
	if total > 0 {
		filler = distinctBarRefiller(baseBarStyle())
	}
	p.logger.Println("Setting bar total:", total)
	bar, err := p.progress.Add(total, filler,
		mpb.BarFillerTrim(),
		mpb.BarPriority(p.id),
		mpb.PrependDecorators(
			newFlashDecorator(newMainDecorator(curTry, p.name, "%s %.1f", decor.WCSyncWidthR), msgCh, 15),
			decor.Conditional(total > 0,
				decor.OnComplete(decor.NewPercentage("%.2f", decor.WCSyncSpace), "100%"),
				decor.OnComplete(decor.Spinner([]string{`-`, `\`, `|`, `/`}, decor.WC{C: decor.DextraSpace}), "100% "),
			),
		),
		mpb.AppendDecorators(
			decor.Conditional(total > 0,
				decor.OnCompleteOrOnAbort(decor.EwmaNormalizedETA(
					decor.ET_STYLE_MMSS,
					30,
					decor.FixedIntervalTimeNormalizer(50),
					decor.WCSyncWidth,
				), ""),
				nil,
			),
			decor.Conditional(total > 0,
				decor.OnCompleteOrOnAbort(decor.Name("", decor.WCSyncWidth), ":"),
				nil,
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
			err := fmt.Errorf("%q size mismatch: expected %d got %d", stat.Name(), p.Written, size)
			return withStack(err)
		}
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
	var dtt int // decrement timeout threshold
	var curTry uint32
	var buffer [bufMax]byte

	bufLen := int(min(bufMax, bufSize*1024))
	consecutiveResetOk := 32 / int(bufSize)
	timeout := initialTimeout
	barMsg := make(chan string, 1)
	trace := &httptrace.ClientTrace{
		GotConn: func(connInfo httptrace.GotConnInfo) {
			p.logger.Println("Connection RemoteAddr:", connInfo.Conn.RemoteAddr())
		},
	}
	isUnexpectedEOF := func(err error) (unexpectedEOF bool) {
		defer func() {
			p.logger.Printf("IsUnexpectedEOF: %t", unexpectedEOF)
		}()
		return errors.Is(err, io.ErrUnexpectedEOF)
	}

	p.logger.Println("ReadFull buf len:", bufLen)

	return backoff.RetryWithContext(p.ctx, exponential.New(exponential.WithBaseDelay(500*time.Millisecond)),
		func(attempt uint, backoffReset func()) (retry bool, err error) {
			ctx, cancel := context.WithCancel(p.ctx)
			timer := time.AfterFunc(timeout, func() {
				cancel()
				p.logger.Println("Timer has expired")
			})
			var idle time.Duration
			start := time.Now()
			defer func(written int64) {
				if !timer.Stop() {
					timeout += 5 * time.Second
					timeout = min(timeout, maxTimeout*time.Second)
					dtt += consecutiveResetOk
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
				go func(prefix string, bar *mpb.Bar) {
					if errors.Is(ctx.Err(), context.Canceled) {
						prefix += timeoutMsg
						if bar != nil {
							barMsg <- fmt.Sprintf("%s %s", p.name, timeoutMsg)
						}
					} else if prefix != "" {
						prefix = prefix[:len(prefix)-1]
					}
					_, _ = fmt.Fprintln(p.progress, prefix, unwrapOrErr(err).Error())
				}(p.logger.Prefix(), bar)
				p.logger.SetPrefix(fmt.Sprintf(p.prefixFormat, attempt+1))
				atomic.StoreUint32(&curTry, uint32(attempt+1))
			}(p.Written)

			p.logger.Printf("GET(timeout=%s,dtt=%d): %s", timeout, dtt, req.URL)

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
					err = cmp.Or(err, resp.Body.Close())
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
				select {
				case <-p.status.ctx.Done():
					var fallback singleModeFallback
					if errors.As(context.Cause(p.status.ctx), &fallback) {
						// StatusPartialContent after StatusOK
						panic(UnexpectedHttpStatus(http.StatusPartialContent))
					}
				default:
					p.status.cancel(nil)
				}
				if p.file == nil {
					p.file, err = os.OpenFile(p.output, os.O_WRONLY|os.O_CREATE|os.O_APPEND, umask)
					if err != nil {
						return false, withStack(err)
					}
					bar, err = p.newBar(&curTry, barMsg)
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
					p.status.cancel(singleModeFallback(p.id))
					p.single = true
					if resp.ContentLength > 0 {
						p.Stop = resp.ContentLength - 1
					}
					if p.file == nil {
						p.file, err = os.OpenFile(p.output, os.O_WRONLY|os.O_CREATE|os.O_TRUNC, umask)
						if err != nil {
							return false, withStack(err)
						}
						bar, err = p.newBar(&curTry, barMsg)
						if err != nil {
							return false, withStack(err)
						}
					}
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
				return true, withStack(UnexpectedHttpStatus(resp.StatusCode))
			default:
				if attempt != 0 {
					atomic.AddUint32(&globTry, ^uint32(0))
				}
				err := UnexpectedHttpStatus(resp.StatusCode)
				_, _ = fmt.Fprintf(p.progress, "%s%s\n", p.logger.Prefix(), err.Error())
				if bar != nil {
					bar.Abort(!p.single)
				}
				return false, withStack(err)
			}

			var limit func(limitTimer, context.Context) bool

			// io.ReadFull returns io.ErrUnexpectedEOF if an io.EOF happens after reading some but not all the bytes
			// therefore we enter loop on io.ErrUnexpectedEOF in order to force io.ReadFull to return io.EOF
			for n := bufLen; timer.Reset(timeout+sleep) && n == bufLen || isUnexpectedEOF(err); {
				start := time.Now()
				n, err = io.ReadFull(resp.Body, buffer[:bufLen])
				rDur := time.Since(start)
				if n == 0 {
					// n is zero either on context timeout or on io.EOF
					// accumulating zero dur for ewma decorators
					bar.EwmaIncrBy(n, rDur)
					continue
				}

				var timer limitTimer
				if sleep != 0 {
					timer.timer = time.NewTimer(sleep)
					limit = limitTimer.wait
				} else {
					limit = limitTimer.nop
				}

				if _, err := p.file.Write(buffer[:n]); err != nil {
					timer.stop()
					return false, withStack(cmp.Or(p.file.Truncate(p.Written), err))
				}

				p.Written += int64(n)

				if !p.single {
					p.progress.total <- n
				} else if p.total() <= 0 {
					bar.SetTotal(p.Written, false)
				}

				if timeout != initialTimeout {
					switch dtt {
					case 0:
						timeout -= 5 * time.Second
						timeout = max(timeout, initialTimeout)
						if timeout == initialTimeout {
							backoffReset()
						}
					default:
						dtt--
					}
				}

				bar.EwmaIncrBy(n, rDur)

				if limit(timer, ctx) {
					idle += sleep
					bar.EwmaIncrBy(0, sleep)
				}
			}

			if errors.Is(err, io.EOF) {
				if p.total() <= 0 {
					p.Stop = p.Written - 1 // make sure next p.total() result is never negative
					bar.EnableTriggerComplete()
				}
				if !p.isDone() {
					return false, withStack(errors.New("part is not done after EOF"))
				}
				p.logger.Println("Part is done")
				return false, nil
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

type limitTimer struct {
	timer *time.Timer
}

func (t limitTimer) stop() {
	if t.timer != nil {
		t.timer.Stop()
	}
}

// wait invariant: t.timer != nil
func (t limitTimer) wait(ctx context.Context) bool {
	select {
	case <-t.timer.C:
		return true
	case <-ctx.Done():
		t.timer.Stop()
		return false
	}
}

func (limitTimer) nop(context.Context) bool {
	return false
}

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
const prefixFormat = "[%s:R%02d] "

var globTry uint32

type firstHttpResponseContext struct {
	ctx    context.Context
	cancel context.CancelCauseFunc
	id     chan int
}

// Part represents state of each download part
type Part struct {
	Start   int64
	Stop    int64
	Written int64

	id        int
	ctx       context.Context
	cancel    context.CancelFunc
	client    *http.Client              // shared among parts
	progress  *progress                 // shared among parts
	firstResp *firstHttpResponseContext // shared among parts
	patcher   func(*http.Request)       // shared among parts
	logger    *log.Logger
	file      *os.File
	name      string
	output    string
	single    bool
}

type flashBar struct {
	*mpb.Bar
	signal chan<- struct{}
}

func (b *flashBar) flashTimeout() {
	b.signal <- struct{}{}
}

func (b *flashBar) Abort(drop bool) {
	if b != nil {
		b.Bar.Abort(drop)
	}
}

func (p Part) newBar(curTry *uint32) (*flashBar, error) {
	var filler mpb.BarFiller
	total := p.total()
	if total > 0 {
		filler = distinctBarRefiller(baseBarStyle())
	}
	p.logger.Println("Setting bar total:", total)
	msg, ch := fmt.Sprintf("%s %s", p.name, timeoutMsg), make(chan struct{}, 1)
	bar, err := p.progress.Add(total, filler,
		mpb.BarFillerTrim(),
		mpb.BarPriority(p.id),
		mpb.PrependDecorators(
			newFlashDecorator(newMainDecorator(curTry, p.name, "%s %.1f", decor.WCSyncWidthR), msg, ch),
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
	return &flashBar{bar, ch}, nil
}

func (p *Part) init(id int, session *Session) error {
	p.id = id
	p.name = fmt.Sprintf("P%02d", id)
	p.output = fmt.Sprintf("%s.%02d", session.OutputName, id)
	p.single = session.Single
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

func (p *Part) download(
	debugw io.Writer,
	location string,
	bufSize, maxTry uint,
	sleep, initialTimeout time.Duration,
) (err error) {
	var totalElapsed, totalIdle time.Duration
	defer func() {
		p.cancel()
		p.logger.Println("Total Written:", p.Written)
		p.logger.Println("Total Elapsed:", totalElapsed)
		p.logger.Println("Total Idle:", totalIdle)
		err = withMessage(err, p.name)
	}()

	p.logger = log.New(debugw, fmt.Sprintf(prefixFormat, p.name, 0), log.LstdFlags)

	req, err := http.NewRequest(http.MethodGet, location, nil)
	if err != nil {
		return withStack(err)
	}
	if p.patcher != nil {
		p.patcher(req)
	}

	var bar *flashBar
	var dtt int // decrement timeout threshold
	var curTry uint32
	var partial bool
	var buffer [bufMax]byte

	bufLen := int(min(bufMax, bufSize*1024))
	consecutiveResetOk := 32 / int(bufSize)
	timeout := initialTimeout
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
					bar.Abort(!p.single)
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
					bar.Abort(!p.single)
					return
				}
				p.logger.Println("Retry reason:", err.Error())
				go func(prefix string, flash bool) {
					if errors.Is(ctx.Err(), context.Canceled) {
						prefix += timeoutMsg
						if flash {
							bar.flashTimeout()
						}
					} else if prefix != "" {
						prefix = prefix[:len(prefix)-1]
					}
					_, _ = fmt.Fprintln(p.progress, prefix, unwrapOrErr(err).Error())
				}(p.logger.Prefix(), bar != nil)
				p.logger.SetPrefix(fmt.Sprintf(prefixFormat, p.name, attempt+1))
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

			p.logger.Println("Response Status:", resp.Status)

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
				case p.firstResp.id <- p.id:
					p.firstResp.cancel(modePartial)
					partial = true
				default:
					if !partial && errors.Is(context.Cause(p.firstResp.ctx), modeFallback) {
						// some other part got http.StatusOK first
						panic(UnexpectedHttpStatus(http.StatusPartialContent))
					}
					partial = true
				}
				if p.file == nil {
					p.file, err = os.OpenFile(p.output, os.O_WRONLY|os.O_CREATE|os.O_APPEND, umask)
					if err != nil {
						return false, withStack(err)
					}
				}
				if bar == nil {
					bar, err = p.newBar(&curTry)
					if err != nil {
						return false, withStack(err)
					}
				}
				if p.Written != 0 {
					go bar.SetRefill(p.Written)
				}
			case http.StatusOK: // no partial content, fallback to single part mode
				select {
				case p.firstResp.id <- p.id:
					p.firstResp.cancel(modeFallback)
					p.single = true
					p.Start, p.Stop = 0, resp.ContentLength-1
					if p.Written != 0 {
						panic(fmt.Errorf("unexpected written %d on first %s", p.Written, resp.Status))
					}
					p.file, err = os.OpenFile(p.output, os.O_WRONLY|os.O_CREATE|os.O_TRUNC, umask)
					if err != nil {
						return false, withStack(err)
					}
					bar, err = p.newBar(&curTry)
					if err != nil {
						return false, withStack(err)
					}
				default:
					if !p.single || partial {
						err := context.Cause(p.firstResp.ctx)
						if errors.Is(err, modePartial) {
							// some other part got http.StatusPartialContent first
							panic(UnexpectedHttpStatus(http.StatusOK))
						}
						// some other part got http.StatusOK already
						p.logger.Printf("Quit: %v", err)
						return false, nil
					}
					if p.Written != 0 {
						// there is no way to resume on http.StatusOK so retry from scratch
						err := p.file.Truncate(0)
						if err != nil {
							return false, withStack(err)
						}
						p.Written = 0
						bar.SetCurrent(0)
					}
				}
			case http.StatusInternalServerError, http.StatusNotImplemented, http.StatusBadGateway, http.StatusServiceUnavailable, http.StatusGatewayTimeout:
				return true, withStack(UnexpectedHttpStatus(resp.StatusCode))
			default:
				if attempt != 0 {
					atomic.AddUint32(&globTry, ^uint32(0))
				}
				err := UnexpectedHttpStatus(resp.StatusCode)
				_, _ = fmt.Fprintf(p.progress, "%s%s\n", p.logger.Prefix(), err.Error())
				return false, withStack(err)
			}

			var limit func(limitTimer, context.Context) bool
			isUnexpectedEOF := makeUnexpectedEOFFuser(p.logger)

			// io.ReadFull returns io.ErrUnexpectedEOF if an io.EOF happens after reading
			// some but not all the bytes therefore to force io.ReadFull to return io.EOF
			// loop is entered one more time on first io.ErrUnexpectedEOF encounter
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

			if p.total() <= 0 && errors.Is(err, io.EOF) {
				p.Stop = p.Written - 1 // make sure next p.total() result is never negative
				bar.EnableTriggerComplete()
			}

			if p.isDone() {
				p.logger.Println("Part is done:", err)
				if errors.Is(err, io.EOF) {
					return false, nil
				}
				panic(fmt.Errorf("expected EOF, got: %w", err))
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

func makeUnexpectedEOFFuser(logger *log.Logger) func(error) bool {
	var fused bool
	return func(err error) (unexpectedEOF bool) {
		defer func() {
			fused = cmp.Or(fused, unexpectedEOF)
			logger.Printf("IsUnexpectedEOF: %t", unexpectedEOF)
		}()
		return errors.Is(err, io.ErrUnexpectedEOF) && !fused
	}
}

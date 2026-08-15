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
	"path/filepath"
	"sync/atomic"
	"time"

	"github.com/vbauerster/backoff"
	"github.com/vbauerster/backoff/exponential"
	"github.com/vbauerster/mpb/v8"
	"github.com/vbauerster/mpb/v8/decor"
)

const bufMax = 16 * 1024
const timeoutMsg = "Timeout..."
const prefixFormat = "[%s:R%02d] "
const ewmaAge = 31

var globTry atomic.Uint32

// Part represents state of each download part
type Part struct {
	Id      uint
	Start   int64
	Stop    int64
	Written int64

	name      string
	patcher   httpRequestPatcher
	ctx       context.Context
	cancel    context.CancelFunc
	curTry    *atomic.Uint32
	progress  *progress                 // shared among parts
	firstResp *firstHttpResponseContext // shared among parts
	logger    *log.Logger
	output    *outFile
	single    bool
}

type firstHttpResponseContext struct {
	ctx    context.Context
	cancel context.CancelCauseFunc
	id     chan uint
}

// newFirstHttpResponseContext invariant: id must not be depleted until all parts done.
func newFirstHttpResponseContext(parent context.Context) *firstHttpResponseContext {
	ctx, cancel := context.WithCancelCause(parent)
	return &firstHttpResponseContext{
		ctx:    ctx,
		cancel: cancel,
		id:     make(chan uint, 1),
	}
}

type downloadOptions struct {
	bufSize uint
	maxTry  uint
	timeout time.Duration
	sleep   time.Duration
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

func (p Part) newBar() (*flashBar, error) {
	total, signal := p.len(), make(chan struct{}, 1)
	p.logger.Println("Setting bar total:", total)
	bar, err := p.progress.Add(total, barBuilder.Build(),
		mpb.BarFillerTrim(),
		mpb.BarPriority(int(p.Id)),
		mpb.PrependDecorators(
			newFlashDecorator(
				newMainDecorator(p.curTry, p.name, "%s %.1f", decor.WCSyncWidthR),
				fmt.Sprintf("%s %s", p.name, timeoutMsg),
				signal,
			),
			decor.Conditional(total > 0,
				decor.OnComplete(decor.NewPercentage("%.2f", decor.WCSyncSpace), "100%"),
				decor.OnComplete(decor.Spinner([]string{`-`, `\`, `|`, `/`}, decor.WC{C: decor.DextraSpace}), "100%"),
			),
		),
		mpb.AppendDecorators(
			decor.Conditional(total > 0,
				decor.OnCompleteOrOnAbort(decor.EwmaNormalizedETA(
					decor.ET_STYLE_MMSS,
					ewmaAge,
					decor.FixedIntervalTimeNormalizer(50),
					decor.WCSyncWidth,
				), "peak:"),
				decor.OnCompleteOrOnAbort(decor.Name("", decor.WCSyncWidth), "peak:"),
			),
			newEwmaSpeedPeak("%.1f", ewmaAge, decor.WCSyncSpace),
		),
	)
	if err != nil {
		return nil, err
	}
	if p.Written != 0 {
		p.logger.Println("Setting bar current:", p.Written)
		bar.SetCurrent(p.Written)
		bar.SetRefillCurrent()
	}
	return &flashBar{bar, signal}, nil
}

func (p *Part) init(session *Session) error {
	p.name = fmt.Sprintf("P%02d", p.Id)
	p.curTry = new(atomic.Uint32)
	p.single = session.Single
	p.output = &outFile{
		name: filepath.Join(session.dir, fmt.Sprintf("%s.%02d", session.OutputName, p.Id)),
	}
	if session.restored && p.Written != 0 {
		stat, err := p.output.Stat()
		if err != nil {
			return withStack(err)
		}
		size := stat.Size()
		if size != p.Written {
			err := fmt.Errorf("%q size mismatch: expected %d got %d", p.output, p.Written, size)
			return withStack(err)
		}
	}
	return nil
}

func (p *Part) download(location string, opt downloadOptions) (err error) {
	var bar *flashBar
	var totalElapsed, totalIdle time.Duration
	defer func() {
		p.cancel()
		bar.Abort(!p.single)
		p.logger.Println("Total Written:", p.Written)
		p.logger.Println("Total Elapsed:", totalElapsed)
		p.logger.Println("Total Idle:", totalIdle)
		p.logger.Println("Return err:", err)
		err = withMessage(err, p.name)
	}()

	req, err := http.NewRequest(http.MethodGet, location, nil)
	if err != nil {
		return withStack(err)
	}

	if p.patcher != nil {
		p.patcher.patch(req)
	}

	var buffer [bufMax]byte
	var dtt int // decrement timeout threshold
	var partial bool

	consecutiveResetOk := 32 / int(opt.bufSize)
	timeout := opt.timeout
	trace := &httptrace.ClientTrace{
		GotConn: func(connInfo httptrace.GotConnInfo) {
			p.logger.Println("Connection RemoteAddr:", connInfo.Conn.RemoteAddr())
		},
	}

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
				written = p.Written - written
				p.logger.Println("Written:", written)
				p.logger.Println("Elapsed:", elapsed)
				p.logger.Println("Idle:", idle)
				if !retry || err == nil || p.ctx.Err() != nil {
					return
				}
				switch attempt {
				case 0:
					globTry.Add(1)
				case opt.maxTry:
					globTry.Add(^uint32(0)) // decrement
					retry, err = false, withStack(ErrMaxRetry)
					_, _ = fmt.Fprintf(p.progress, "%s%s (%.1f / %.1f)\n",
						p.logger.Prefix(),
						err.Error(),
						decor.SizeB1024(p.Written),
						decor.SizeB1024(p.len()))
					return
				}
				go func(prefix string, isBarOk, partial bool) {
					if errors.Is(ctx.Err(), context.Canceled) {
						if isBarOk {
							bar.flashTimeout()
						}
						_, _ = fmt.Fprintln(p.progress, prefix+timeoutMsg, context.Canceled.Error())
					} else {
						_, _ = fmt.Fprintln(p.progress, prefix+unwrapOrErr(err).Error())
					}
					if isBarOk && partial && written != 0 {
						bar.SetRefillCurrent()
					}
				}(p.logger.Prefix(), bar != nil, partial)
				p.logger.Println("Retry err:", err.Error())
				p.logger.SetPrefix(fmt.Sprintf(prefixFormat, p.name, attempt+1))
				p.curTry.Store(uint32(attempt + 1))
			}(p.Written)

			p.logger.Printf("GET(timeout=%s,dtt=%d): %s", timeout, dtt, req.URL)

			req.Header.Set(hRange, p.getRange())
			for k, v := range req.Header {
				p.logger.Printf("Request Header: %s: %v", k, v)
			}

			resp, err := httpClient.Do(req.WithContext(httptrace.WithClientTrace(ctx, trace)))
			if err != nil {
				return true, withStack(err)
			}
			defer func() {
				if resp.Body != nil {
					err = cmp.Or(err, resp.Body.Close())
				}
			}()

			p.logger.Println("Response Status:", resp.Status)

			if jar := httpClient.Jar; jar != nil {
				for _, cookie := range jar.Cookies(req.URL) {
					p.logger.Println("Cookie:", cookie) // *http.Cookie implements fmt.Stringer
				}
			}

			for k, v := range resp.Header {
				p.logger.Printf("Response Header: %s: %v", k, v)
			}

			switch resp.StatusCode {
			case http.StatusPartialContent:
				select {
				case p.firstResp.id <- p.Id:
					p.firstResp.cancel(errContextPartial)
				default:
					if !partial && errors.Is(context.Cause(p.firstResp.ctx), errContextFallback) {
						// some other part got http.StatusOK first
						panic(UnexpectedHttpStatusError(http.StatusPartialContent))
					}
				}
				if p.output.file == nil {
					err := p.output.Open(os.O_WRONLY | os.O_CREATE | os.O_APPEND)
					if err != nil {
						return false, withStack(err)
					}
					bar, err = p.newBar()
					if err != nil {
						return false, withStack(err)
					}
					partial = true
				}
			case http.StatusOK: // no partial content, fallback to single part mode
				select {
				case p.firstResp.id <- p.Id:
					p.firstResp.cancel(errContextFallback)
					if p.Written != 0 {
						panic(fmt.Errorf("unexpected written %d on first %s", p.Written, resp.Status))
					}
					err := p.output.Open(os.O_WRONLY | os.O_CREATE | os.O_TRUNC)
					if err != nil {
						return false, withStack(err)
					}
					bar, err = p.newBar()
					if err != nil {
						return false, withStack(err)
					}
					p.reset(resp.ContentLength)
				default:
					if !p.single || partial {
						if errors.Is(context.Cause(p.firstResp.ctx), errContextPartial) {
							// some other part got http.StatusPartialContent first
							panic(UnexpectedHttpStatusError(http.StatusOK))
						}
						p.logger.Println("Some other part got:", resp.Status)
						return false, nil
					}
					if p.Written != 0 {
						// there is no way to resume on http.StatusOK so retry from scratch
						err := p.output.Truncate(0)
						if err != nil {
							return false, withStack(err)
						}
						p.Written = 0
						bar.SetCurrent(0)
					}
				}
			case http.StatusInternalServerError, http.StatusNotImplemented, http.StatusBadGateway, http.StatusServiceUnavailable, http.StatusGatewayTimeout:
				return true, withStack(UnexpectedHttpStatusError(resp.StatusCode))
			default:
				if attempt != 0 {
					globTry.Add(^uint32(0)) // decrement
				}
				err := UnexpectedHttpStatusError(resp.StatusCode)
				_, _ = fmt.Fprintf(p.progress, "%s%s\n", p.logger.Prefix(), err.Error())
				return false, withStack(err)
			}

			var limit func(limitTimer, context.Context) bool
			isUnexpectedEOF := makeUnexpectedEOFFuser(p.logger)

			buf := buffer[:min(bufMax, opt.bufSize*1024)]
			// io.ReadFull returns io.ErrUnexpectedEOF if an io.EOF happens after reading
			// some but not all the bytes therefore to force io.ReadFull to return io.EOF
			// loop is entered one more time on first io.ErrUnexpectedEOF encounter
			for n := len(buf); timer.Reset(timeout+opt.sleep) && n == len(buf) || isUnexpectedEOF(err); {
				start := time.Now()
				n, err = io.ReadFull(resp.Body, buf)
				rDur := time.Since(start)
				if n == 0 {
					// n is zero either on context timeout or on io.EOF
					// accumulating zero dur for ewma decorators
					bar.EwmaIncrBy(n, rDur)
					continue
				}

				var timer limitTimer
				if opt.sleep != 0 {
					timer.timer = time.NewTimer(opt.sleep)
					limit = limitTimer.wait
				} else {
					limit = limitTimer.nop
				}

				if _, err := p.output.Write(buf[:n]); err != nil {
					timer.stop()
					return false, withStack(cmp.Or(p.output.Truncate(p.Written), err))
				}

				p.Written += int64(n)

				if !p.single {
					p.progress.incrTotal(n)
				} else if p.len() <= 0 {
					bar.SetTotal(p.Written, false)
				}

				if timeout != opt.timeout {
					switch dtt {
					case 0:
						timeout -= 5 * time.Second
						timeout = max(timeout, opt.timeout)
						if timeout == opt.timeout {
							backoffReset()
						}
					default:
						dtt--
					}
				}

				bar.EwmaIncrBy(n, rDur)

				if limit(timer, ctx) {
					idle += opt.sleep
					bar.EwmaIncrBy(0, opt.sleep)
				}
			}

			if p.len() <= 0 && errors.Is(err, io.EOF) {
				p.reset(p.Written)
				bar.EnableTriggerComplete()
			}

			if p.isDone() {
				if errors.Is(err, io.EOF) {
					p.logger.Println("Part is done")
					return false, nil
				}
				panic(fmt.Errorf("expected EOF, got: %w", err))
			}

			// err is never nil here
			return true, err
		})
}

func (p *Part) reset(contentLength int64) {
	p.single = true
	p.Start, p.Stop = 0, contentLength-1
}

func (p Part) getRange() string {
	if p.Stop <= 0 {
		return "bytes=0-"
	}
	return fmt.Sprintf("bytes=%d-%d", p.Start+p.Written, p.Stop)
}

// on ContentLength =  0 p.Stop is -1 and len evaluates to  0
// on ContentLength = -1 p.Stop is -2 and len evaluates to -1
func (p Part) len() int64 {
	return p.Stop - p.Start + 1
}

func (p Part) isDone() bool {
	return p.Written == p.len()
}

func (p Part) isContentDownloaded() bool {
	return p.Written != 0 && p.isDone()
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

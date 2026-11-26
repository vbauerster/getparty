package getparty

import (
	"cmp"
	"context"
	"errors"
	"expvar"
	"fmt"
	"io"
	"log"
	"net/http"
	"net/http/httptrace"
	"os"
	"sync/atomic"
	"time"

	"github.com/vbauerster/backoff/v2"
	"github.com/vbauerster/mpb/v8"
	"github.com/vbauerster/mpb/v8/decor"
)

const timeoutMsg = "Timeout..."
const prefixFormat = "[%s:R%02d] "
const ewmaAge = 32

var globTry atomic.Uint32

// Part represents state of each download part
type Part struct {
	Id      uint
	Start   int64
	Stop    int64
	Written int64

	name      string
	ctx       context.Context
	curTry    *atomic.Uint32
	progress  *progress               // shared among parts
	firstResp *sessionResponseContext // shared among parts
	logger    *log.Logger
	output    *outFile
	single    bool
}

type sessionResponseContext struct {
	ctx    context.Context
	cancel context.CancelCauseFunc
	id     chan uint
}

// newSessionResponseContext invariant: id must not be depleted until all parts done.
func newSessionResponseContext(parent context.Context) *sessionResponseContext {
	ctx, cancel := context.WithCancelCause(parent)
	return &sessionResponseContext{
		ctx:    ctx,
		cancel: cancel,
		id:     make(chan uint, 1),
	}
}

type downloadOptions struct {
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

func (p Part) newBar(total int64) (*flashBar, error) {
	signal := make(chan struct{}, 1)
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

func (p *Part) init(outputName string) error {
	p.output = &outFile{
		name: fmt.Sprintf("%s.%02d", outputName, p.Id),
	}
	if p.Written != 0 {
		return withStack(p.output.checkContentMismatch(p.Written))
	}
	return nil
}

func (p *Part) download(patcher httpRequestPatcher, location string, opt downloadOptions, buf []byte) (err error) {
	var totalElapsed, totalIdle time.Duration
	defer func() {
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

	if patcher != nil {
		patcher.patch(req)
	}

	var bar *flashBar
	var statusPartial bool
	var statusOK bool
	var progressLogger *log.Logger
	trace := &httptrace.ClientTrace{
		GotConn: func(connInfo httptrace.GotConnInfo) {
			p.logger.Println("Connection RemoteAddr:", connInfo.Conn.RemoteAddr())
		},
	}

	scoreManager := make(chunkScoreManager, 1)
	defer func() {
		close(scoreManager)
		if bar != nil {
			bar.Abort(!p.single)
		}
	}()

	go scoreManager.serve(p.logger)

	if buf == nil {
		buf = make([]byte, bufSize)
	}

	timeout2 := opt.timeout * 2
	strategy := backoff.DefaultStrategy()

	return backoff.RetryWithContext(p.ctx, strategy, func(attempt uint) (retry bool, err error) {
		chunk := scoreManager.get()
		defer func() {
			scoreManager.put(chunk)
		}()
		p.logger.Printf("LimitRead=%dKiB timeout=%s", chunk.size, opt.timeout)
		timedCtx, cancel := context.WithCancelCause(p.ctx)
		timer := time.AfterFunc(opt.timeout, func() {
			cancel(errTimeout)
		})
		var idle time.Duration
		defer func(written int64, start time.Time) {
			timer.Stop()
			cancel(nil)
			elapsed := time.Since(start)
			totalElapsed += elapsed
			totalIdle += idle
			p.logger.Println("Written:", p.Written-written)
			p.logger.Println("Elapsed:", elapsed)
			p.logger.Println("Idle:", idle)
			p.logger.Println("Retry err:", err)
			if !retry || err == nil || p.ctx.Err() != nil {
				return
			}
			if prefix := p.logger.Prefix(); progressLogger == nil {
				progressLogger = log.New(p.progress, prefix, log.Ltime)
			} else {
				progressLogger.SetPrefix(prefix)
			}
			switch attempt {
			case opt.maxTry:
				globTry.Add(^uint32(0)) // decrement
				progressLogger.Printf("%s (%.1f / %.1f)",
					ErrMaxRetry.Error(),
					decor.SizeB1024(p.Written),
					decor.SizeB1024(p.len()))
				retry, err = false, withStack(ErrMaxRetry)
			case 0:
				globTry.Add(1)
				fallthrough
			default:
				p.curTry.Store(uint32(attempt + 1))
				p.logger.SetPrefix(fmt.Sprintf(prefixFormat, p.name, attempt+1))
				if chunk.size == 8 && p.Written-written == 0 {
					opt.timeout = min(opt.timeout+time.Second, timeout2, maxTimeout*time.Second)
				}
				go func(barNotNil bool) {
					if errors.Is(context.Cause(timedCtx), errTimeout) {
						if barNotNil {
							bar.flashTimeout()
						}
						progressLogger.Println(errTimeout.Error())
					} else {
						progressLogger.Println(unwrapOrErr(err).Error())
					}
				}(bar != nil)
			}
		}(p.Written, time.Now())

		p.logger.Println("GET:", req.URL)

		req.Header.Set(hRange, p.getRange())
		for k, v := range req.Header {
			p.logger.Printf("Request Header: %s: %v", k, v)
		}

		resp, err := httpClient.Do(req.WithContext(httptrace.WithClientTrace(timedCtx, trace)))
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
				p.firstResp.cancel(errResponsePartial)
				statusPartial = true
			case <-p.firstResp.ctx.Done():
				if !statusPartial {
					if errors.Is(context.Cause(p.firstResp.ctx), errResponseOK) {
						// some other part got http.StatusOK first
						panic(UnexpectedHttpStatusError(http.StatusPartialContent))
					}
					statusPartial = true
				}
			}
			if p.output.file == nil {
				err := p.output.Open(os.O_WRONLY | os.O_CREATE | os.O_APPEND)
				if err != nil {
					return false, withStack(err)
				}
			}
			if bar == nil {
				b, err := p.newBar(p.len())
				if err != nil {
					return false, withStack(err)
				}
				bar = b
			} else {
				bar.SetRefillCurrent()
			}
		case http.StatusOK: // no partial content, fallback to single part mode
			select {
			case p.firstResp.id <- p.Id:
				p.firstResp.cancel(errResponseOK)
				if p.Written != 0 {
					panic(fmt.Errorf("unexpected written %d on first %s", p.Written, resp.Status))
				}
				if bar != nil {
					panic(errors.New("expected nil bar"))
				}
				b, err := p.newBar(resp.ContentLength)
				if err != nil {
					return false, withStack(err)
				}
				bar = b
				statusOK = true
				p.single = true
				p.resetLength(resp.ContentLength)
			case <-p.firstResp.ctx.Done():
				if !statusOK && errors.Is(context.Cause(p.firstResp.ctx), errResponsePartial) {
					// some other part got http.StatusPartialContent first
					panic(UnexpectedHttpStatusError(http.StatusPartialContent))
				}
				if !p.single {
					p.logger.Println("Some other part got:", resp.Status)
					return false, nil
				}
				p.Written = 0
				expProgress.Set(expCurrent, expvar.Func(func() any { return 0 }))
				bar.SetCurrent(0)
				if resp.ContentLength != p.len() {
					if progressLogger != nil {
						progressLogger.Println("ContentLength changed:", resp.ContentLength)
					}
					p.logger.Println("ContentLength changed, updating bar total:", resp.ContentLength)
					bar.SetTotal(resp.ContentLength, false)
					bar.EnableTriggerComplete()
					p.resetLength(resp.ContentLength)
				}
			}
			err := p.output.Open(os.O_WRONLY | os.O_CREATE)
			if err != nil {
				return false, withStack(err)
			}
		case http.StatusInternalServerError, http.StatusNotImplemented, http.StatusBadGateway, http.StatusServiceUnavailable, http.StatusGatewayTimeout:
			return true, withStack(UnexpectedHttpStatusError(resp.StatusCode))
		default:
			if attempt != 0 {
				globTry.Add(^uint32(0)) // decrement
			}
			if prefix := p.logger.Prefix(); progressLogger == nil {
				progressLogger = log.New(p.progress, prefix, log.Ltime)
			} else {
				progressLogger.SetPrefix(prefix)
			}
			err := UnexpectedHttpStatusError(resp.StatusCode)
			progressLogger.Println(err.Error())
			return false, withStack(err)
		}

		var nw int64
		var lt limitTimer
		var limit func(limitTimer, context.Context) bool
		var sleep bool
		if opt.sleep == 0 {
			limit = limitTimer.nop
		} else {
			limit = limitTimer.wait
			sleep = true
		}
		readLimit := int64(chunk.size) * 1024
		for reset := makeReset(strategy, timer, attempt); reset(opt.timeout + opt.sleep); {
			start := time.Now()
			// p.output doesn't implement io.ReaderFrom which implies buf will be used to perform the copy
			nw, err = io.CopyBuffer(p.output, io.LimitReader(resp.Body, readLimit), buf)
			ewmaDur := time.Since(start)

			if nw == 0 && err != nil {
				bar.EwmaIncrInt64(0, ewmaDur)
				p.logger.Println("Zero copy:", err.Error())
				if errors.Is(err, io.ErrUnexpectedEOF) {
					continue
				}
				break
			}

			if sleep {
				lt.timer = time.NewTimer(opt.sleep)
			}

			p.Written += nw

			if !p.single {
				p.progress.incrTotal(nw)
			} else {
				expProgress.Add(expCurrent, nw)
				if p.len() <= 0 {
					// bar.SetTotal should precede bar.EwmaIncrInt64
					bar.SetTotal(p.Written, false)
				}
			}

			bar.EwmaIncrInt64(nw, ewmaDur)

			if nw < readLimit && err == nil {
				if lt.timer != nil {
					lt.timer.Stop()
				}
				err = io.EOF // src stopped early; must have been EOF
				p.logger.Println("Break loop:", nw, err.Error())
				break
			}

			chunk.reads++

			if limit(lt, timedCtx) {
				idle += opt.sleep
				bar.EwmaIncrInt64(0, opt.sleep)
			}
		}

		if errors.Is(err, io.EOF) {
			if p.single && p.len() <= 0 {
				p.resetLength(p.Written)
				bar.EnableTriggerComplete()
			}
			if !p.isDone() {
				panic(errors.New("part is not done after EOF"))
			}
			return false, nil
		}

		if p.isDone() {
			panic(errors.New("part is done before EOF"))
		}

		chunk.failure = true

		if err != nil {
			return true, err
		}
		// timer.Reset most likely returned false so return explicit error
		return true, errTimeout
	})
}

func (p *Part) resetLength(contentLength int64) {
	p.Start = 0
	p.Stop = contentLength - 1
}

func (p Part) getRange() string {
	if p.single || p.Stop <= 0 {
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

func makeReset(strategy backoff.Strategy, timer *time.Timer, attempt uint) func(time.Duration) bool {
	if attempt == 0 {
		return timer.Reset
	}
	var count uint
	return func(d time.Duration) bool {
		ok := timer.Reset(d)
		if ok && count == 1 {
			strategy.Reset()
		}
		count++
		return ok
	}
}

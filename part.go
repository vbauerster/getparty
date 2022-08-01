package getparty

import (
	"bytes"
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
	"github.com/vbauerster/mpb/v7"
	"github.com/vbauerster/mpb/v7/decor"
)

const (
	bufSize = 8192
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

	name      string
	order     int
	maxTry    int
	quiet     bool
	jar       http.CookieJar
	totalBar  *mpb.Bar
	transport *http.Transport
	dlogger   *log.Logger
}

func (p Part) makeBar(progress *mpb.Progress, curTry *uint32) (*mpb.Bar, *msgGate) {
	total := p.total()
	if total < 0 {
		total = 0
	}
	p.dlogger.Printf("Bar total: %d", total)
	mg := newMsgGate(p.quiet, p.name, 15)
	bar := progress.New(total,
		mpb.BarFillerBuilderFunc(func() mpb.BarFiller {
			if total == 0 {
				return mpb.NopStyle().Build()
			}
			return mpb.BarStyle().Lbound(" ").Rbound(" ").Build()
		}),
		mpb.BarFillerTrim(),
		mpb.BarPriority(p.order),
		mpb.PrependDecorators(
			newMainDecorator(curTry, "%s %.1f", p.name, mg, decor.WCSyncWidthR),
			decor.OnCondition(
				decor.OnComplete(decor.Spinner([]string{`-`, `\`, `|`, `/`}, decor.WCSyncSpace), "100% "),
				total == 0,
			),
			decor.OnCondition(
				decor.OnComplete(decor.NewPercentage("%.2f", decor.WCSyncSpace), "100%"),
				total != 0,
			),
		),
		mpb.AppendDecorators(
			decor.OnCondition(
				decor.OnComplete(decor.Name(""), "Avg:"),
				total == 0,
			),
			decor.OnCondition(
				decor.OnComplete(
					decor.NewAverageETA(
						decor.ET_STYLE_MMSS,
						time.Now(),
						decor.FixedIntervalTimeNormalizer(30),
						decor.WCSyncWidthR,
					),
					"Avg:",
				),
				total != 0,
			),
			decor.AverageSpeed(decor.UnitKiB, "%.1f", decor.WCSyncSpace),
			decor.OnComplete(decor.Name("", decor.WCSyncSpace), "Peak:"),
			newSpeedPeak("%.1f", decor.WCSyncSpace),
		),
	)
	return bar, mg
}

func (p *Part) download(
	ctx context.Context,
	progress *mpb.Progress,
	req *http.Request,
	timeout uint,
) (err error) {
	defer func() {
		err = errors.Wrap(err, p.name)
	}()

	fpart, err := os.OpenFile(p.FileName, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0644)
	if err != nil {
		return err
	}
	defer func() {
		if err := fpart.Close(); err != nil {
			p.dlogger.Printf("ERR: close %q: %s", fpart.Name(), err.Error())
		}
		if p.Skip {
			p.dlogger.Printf("Removing: %q", fpart.Name())
			if err := os.Remove(p.FileName); err != nil {
				p.dlogger.Printf("ERR: remove %q: %s", fpart.Name(), err.Error())
			}
		}
	}()

	var bar *mpb.Bar
	var mg *msgGate
	var curTry uint32
	var ranDur time.Duration
	resetDur := time.Duration(timeout*2) * time.Second
	barInitDone := make(chan struct{})
	prefix := p.dlogger.Prefix()
	initialTimeout := timeout
	start := time.Now()

	return backoff.Retry(ctx,
		exponential.New(
			exponential.WithReset(resetDur),
			exponential.WithBaseDelay(500*time.Millisecond),
		),
		func(attempt int) (retry bool, err error) {
			pw := p.Written
			defer func() {
				if err != nil {
					p.dlogger.Printf("ERR: %s", err.Error())
				}
				ranDur = time.Since(start)
				if pw != p.Written {
					p.Elapsed += ranDur
				}
				p.dlogger.Printf("Ran dur: %v", ranDur)
				start = time.Now()
			}()

			req.Header.Set(hRange, p.getRange())

			p.dlogger.SetPrefix(fmt.Sprintf("%s[%02d] ", prefix, attempt))
			p.dlogger.Printf("GET %q", req.URL)
			p.dlogger.Printf("%s: %s", hUserAgentKey, req.Header.Get(hUserAgentKey))
			p.dlogger.Printf("%s: %s", hRange, req.Header.Get(hRange))

			if attempt > 0 {
				if ranDur < resetDur {
					timeout += 5
				} else {
					timeout = initialTimeout
				}
				atomic.AddUint32(&globTry, 1)
				atomic.StoreUint32(&curTry, uint32(attempt))
			}

			ctxTimeout := time.Duration(timeout) * time.Second
			ctx, cancel := context.WithCancel(ctx)
			defer cancel()
			timer := time.AfterFunc(ctxTimeout, func() {
				cancel()
				// checking for mg != nil here is a data race
				select {
				case <-barInitDone:
					mg.flash("Timeout...")
				default:
				}
				p.dlogger.Printf("Timeout after: %v", ctxTimeout)
			})
			defer timer.Stop()

			client := &http.Client{
				Transport: p.transport,
				Jar:       p.jar,
			}
			resp, err := client.Do(req.WithContext(ctx))
			if err != nil {
				if attempt+1 == p.maxTry {
					if bar != nil {
						mg.finalFlash(ErrMaxRetry.Error())
						bar.Abort(false)
					}
					return false, errors.WithMessage(ErrMaxRetry, err.Error())
				}
				return true, err
			}

			p.dlogger.Printf("Status: %s", resp.Status)
			p.dlogger.Printf("ContentLength: %d", resp.ContentLength)
			if cookies := p.jar.Cookies(req.URL); len(cookies) != 0 {
				p.dlogger.Println("CookieJar:")
				for _, cookie := range cookies {
					p.dlogger.Printf("  %q", cookie)
				}
			}

			switch resp.StatusCode {
			case http.StatusOK: // no partial content, so download with single part
				if p.order != 1 {
					p.Skip = true
					p.dlogger.Print("no partial content, skipping...")
					return false, nil
				}
				if attempt == 0 && p.totalBar != nil {
					p.totalBar.Abort(true)
				}
				if resp.ContentLength > 0 {
					p.Stop = resp.ContentLength - 1
				}
				p.Written = 0
			case http.StatusForbidden, http.StatusTooManyRequests:
				if mg != nil {
					mg.finalFlash(resp.Status)
				}
				fallthrough
			default:
				if resp.StatusCode != http.StatusPartialContent {
					return false, &HttpError{resp.StatusCode, resp.Status}
				}
			}

			if bar == nil {
				bar, mg = p.makeBar(progress, &curTry)
				close(barInitDone)
			}

			body := bar.ProxyReader(resp.Body)
			defer body.Close()

			if p.Written > 0 {
				bar.SetRefill(p.Written)
				p.dlogger.Printf("Bar refill: %d", p.Written)
				if attempt == 0 {
					bar.SetCurrent(p.Written)
					bar.DecoratorAverageAdjust(time.Now().Add(-p.Elapsed))
				}
			}

			buf := bytes.NewBuffer(make([]byte, 0, bufSize))
			for err == nil && timer.Reset(ctxTimeout) {
				_, err = io.CopyN(buf, body, bufSize)
				if err != nil {
					timer.Stop()
				}
				n, e := io.Copy(fpart, buf)
				if p.totalBar != nil {
					p.totalBar.IncrInt64(n)
				}
				if e != nil {
					p.dlogger.Printf("ERR: write to %q: %s", fpart.Name(), e.Error())
					panic(e)
				}
				p.Written += n
				if p.total() <= 0 {
					bar.SetTotal(p.Written, err == io.EOF)
				}
			}

			p.dlogger.Printf("Written: %d", p.Written-pw)

			if err == io.EOF {
				if total := p.total(); total <= 0 {
					p.Stop = p.Written - 1
				} else if total != p.Written {
					return false, errors.New("unexpected EOF")
				}
				return false, nil
			}

			if attempt+1 == p.maxTry {
				mg.finalFlash(ErrMaxRetry.Error())
				bar.Abort(false)
				return false, ErrMaxRetry
			}

			if resp.StatusCode != http.StatusPartialContent {
				bar.SetCurrent(0)
				bar.SetTotal(0, false)
				if e := fpart.Close(); e == nil {
					fpart, e = os.OpenFile(p.FileName, os.O_WRONLY|os.O_TRUNC, 0644)
					if e != nil {
						panic(e)
					}
				}
			}

			return true, err
		})
}

func (p Part) getRange() string {
	if p.Stop < 1 {
		return "bytes=0-"
	}
	return fmt.Sprintf("bytes=%d-%d", p.Start+p.Written, p.Stop)
}

func (p Part) isDone() bool {
	return p.Written > 0 && p.Written == p.total()
}

func (p Part) total() int64 {
	return p.Stop - p.Start + 1
}

package getparty

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"log"
	"math"
	"net/http"
	"net/url"
	"os"
	"sync/atomic"
	"time"

	"github.com/VividCortex/ewma"
	"github.com/pkg/errors"
	"github.com/vbauerster/backoff"
	"github.com/vbauerster/backoff/exponential"
	"github.com/vbauerster/mpb/v4"
	"github.com/vbauerster/mpb/v4/decor"
)

const (
	bufSize = 1 << 12
)

var (
	ErrGiveUp  = errors.New("give up!")
	ErrNilBody = errors.New("nil body")
)

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
	curTry    int32
	quiet     bool
	dlogger   *log.Logger
	transport *http.Transport
}

func (p *Part) makeBar(total int64, progress *mpb.Progress, gate msgGate) *mpb.Bar {
	etaAge := math.Abs(float64(total))
	if total > bufSize {
		etaAge = float64(total) / float64(bufSize)
	}
	bar := progress.AddBar(total, mpb.BarStyle("|=>-|"),
		mpb.BarPriority(p.order),
		mpb.PrependDecorators(
			newMainDecorator("%s %.2f", p.name, &p.curTry, gate, decor.WCSyncWidth),
			decor.Name(" ["),
			decor.OnComplete(
				decor.NewPercentage("%.2f", decor.WCSyncSpace),
				"100%",
			),
		),
		mpb.AppendDecorators(
			decor.OnComplete(
				decor.MovingAverageETA(
					decor.ET_STYLE_MMSS,
					ewma.NewMovingAverage(etaAge),
					decor.MaxTolerateTimeNormalizer(180*time.Second),
					decor.WCSyncWidth,
				),
				"done!",
			),
			decor.Name(" ]"),
			decor.AverageSpeed(decor.UnitKiB, "% .2f", decor.WCSyncSpace),
		),
	)
	return bar
}

func (p *Part) download(ctx context.Context, progress *mpb.Progress, req *http.Request, timeout uint) (err error) {

	var bar *mpb.Bar
	defer func() {
		if err != nil {
			if bar != nil && !p.isDone() && !p.quiet {
				bar.Abort(false)
			}
			err = errors.WithMessage(err, p.name)
		}
		p.dlogger.Printf("quit: %v", err)
	}()

	fpart, err := os.OpenFile(p.FileName, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0644)
	if err != nil {
		return err
	}
	defer func() {
		if err := fpart.Close(); err != nil {
			p.dlogger.Printf("%q close error: %s", fpart.Name(), err.Error())
		}
		if p.Skip {
			if err := os.Remove(fpart.Name()); err != nil {
				p.dlogger.Printf("%q remove error: %s", fpart.Name(), err.Error())
			}
		}
	}()

	total := p.Stop - p.Start + 1
	mg := newMsgGate(p.quiet)
	bar = p.makeBar(total, progress, mg)
	initialWritten := p.Written
	prefix := p.dlogger.Prefix()

	err = backoff.Retry(ctx,
		exponential.New(exponential.WithBaseDelay(50*time.Millisecond)),
		3*time.Minute,
		func(count int, now time.Time) (retry bool, err error) {
			if count > p.maxTry {
				return false, ErrGiveUp
			}
			if p.isDone() {
				p.dlogger.Println("done in try, quitting...")
				return false, nil
			}

			p.dlogger.SetPrefix(fmt.Sprintf("%s[%02d] ", prefix, count))

			req.Header.Set(hRange, p.getRange())
			p.dlogger.Printf("GET %q", req.URL)
			p.dlogger.Printf("%s: %s", hUserAgentKey, req.Header.Get(hUserAgentKey))
			p.dlogger.Printf("%s: %s", hRange, req.Header.Get(hRange))

			defer func() {
				p.Elapsed += time.Since(now)
			}()

			ctxTimeout := time.Duration(timeout) * time.Second
			if count > 0 {
				ctxTimeout = time.Duration((1<<uint(count-1))*timeout) * time.Second
				if ctxTimeout > time.Hour {
					ctxTimeout = time.Hour
				}
				mg.flash(&message{msg: "retrying..."})
				atomic.StoreInt32(&p.curTry, int32(count))
			} else {
				bar.AdjustAverageDecorators(now)
			}
			p.dlogger.Printf("ctxTimeout: %s", ctxTimeout)

			ctx, cancel := context.WithCancel(ctx)
			defer cancel()
			timer := time.AfterFunc(ctxTimeout, func() {
				msg := "timeout..."
				mg.flash(&message{msg: msg})
				p.dlogger.Print(msg)
				cancel()
			})
			defer timer.Stop()

			client := &http.Client{Transport: p.transport}
			resp, err := client.Do(req.WithContext(ctx))
			if err != nil {
				p.dlogger.Printf("client do: %s", err.Error())
				return true, err
			}

			p.dlogger.Printf("resp.Status: %s", resp.Status)
			p.dlogger.Printf("resp.ContentLength: %d", resp.ContentLength)

			if resp.StatusCode == http.StatusOK {
				// no partial content, so download with single part
				if p.order > 0 {
					p.Skip = true
					bar.Abort(true)
					p.dlogger.Print("no partial content, skipping...")
					return false, nil
				}
				total = resp.ContentLength
				bar.SetTotal(total, false)
				p.Stop = total - 1
				p.dlogger.Printf("resetting written: %d", p.Written)
				p.Written = 0
			} else if resp.StatusCode != http.StatusPartialContent {
				return false, errors.Errorf("unexpected status: %s", resp.Status)
			}

			body := resp.Body
			if !p.quiet {
				body = bar.ProxyReader(resp.Body)
				if p.Written > 0 {
					p.dlogger.Printf("bar refill written: %d", p.Written)
					bar.SetRefill(p.Written)
					if p.Written-initialWritten == 0 {
						bar.IncrBy(int(p.Written), p.Elapsed)
						bar.AdjustAverageDecorators(time.Now().Add(-p.Elapsed))
					}
				}
			} else {
				bar.Abort(true)
			}
			if body == nil {
				return false, ErrNilBody
			}
			defer body.Close()

			pWrittenSnap := p.Written
			buf, max := bytes.NewBuffer(make([]byte, 0, bufSize)), int64(bufSize)
			var n int64
			for timer.Reset(ctxTimeout) {
				n, err = io.CopyN(buf, body, max)
				if err != nil {
					p.dlogger.Printf("CopyN err: %s", err.Error())
					if e, ok := err.(*url.Error); ok {
						mg.flash(&message{
							msg: fmt.Sprintf("%.30s..", e.Err.Error()),
						})
						if e.Temporary() {
							max -= n
							continue
						}
					}
					break
				}
				n, _ = io.Copy(fpart, buf)
				p.Written += n
				if total <= 0 && !p.quiet {
					bar.SetTotal(p.Written+max*2, false)
				}
				max = bufSize
			}

			n, _ = io.Copy(fpart, buf)
			p.Written += n
			p.dlogger.Printf("total written: %d", p.Written-pWrittenSnap)
			if total <= 0 {
				p.Stop = p.Written - 1
			}

			if err == io.EOF {
				return false, nil
			}
			return !p.isDone(), err
		})

	if err == ErrGiveUp {
		done := make(chan struct{})
		mg.flash(&message{
			msg:   err.Error(),
			final: true,
			done:  done,
		})
		<-done
	}

	return err
}

func (p Part) getRange() string {
	if p.Stop <= 0 {
		return "bytes=0-"
	}
	return fmt.Sprintf("bytes=%d-%d", p.Start+p.Written, p.Stop)
}

func (p Part) isDone() bool {
	return p.Skip || p.Written > p.Stop-p.Start
}

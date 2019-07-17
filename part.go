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
	"github.com/vbauerster/mpb/v4"
	"github.com/vbauerster/mpb/v4/decor"
)

const (
	bufSize      = 1 << 12
	timeoutIncBy = 5
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
			newMainDecorator("%s % .1f", p.name, &p.curTry, gate, decor.WCSyncWidth),
			decor.Name(" ["),
			decor.Percentage(decor.WCSyncSpace),
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

func (p *Part) download(ctx context.Context, progress *mpb.Progress, req *http.Request, ctxTimeout uint) (err error) {

	var bar *mpb.Bar
	defer func() {
		if err != nil {
			if !p.isDone() && bar != nil {
				bar.Abort(false)
			}
			// just add method name, without stack trace at the point
			err = errors.WithMessage(err, "download: "+p.name)
		}
		p.dlogger.Printf("quit: %v", err)
	}()

	fpart, err := os.OpenFile(p.FileName, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0644)
	if err != nil {
		return err
	}
	defer func() {
		if err := fpart.Close(); err != nil {
			p.dlogger.Printf("%q close error: %v", fpart.Name(), err)
		}
		if p.Skip {
			if err := os.Remove(fpart.Name()); err != nil {
				p.dlogger.Printf("%q remove error: %v", fpart.Name(), err)
			}
		}
	}()

	bOff := backoff.New(
		backoff.WithBaseDelay(40*time.Millisecond),
		backoff.WithResetDelay(2*time.Minute),
	)

	total := p.Stop - p.Start + 1
	mg := newMsgGate(p.quiet)
	bar = p.makeBar(total, progress, mg)
	initialWritten := p.Written
	prefixSnap := p.dlogger.Prefix()

	err = try(func(attempt int) (retry bool, err error) {
		if attempt > p.maxTry {
			return false, ErrGiveUp
		}
		if p.isDone() {
			p.dlogger.Println("done in try, quitting...")
			return false, nil
		}

		p.dlogger.SetPrefix(fmt.Sprintf("%s[%02d] ", prefixSnap, attempt))

		dur := bOff.Backoff(attempt + 1)
		startTimer := time.NewTimer(dur)

		req.Header.Set(hRange, p.getRange())
		p.dlogger.Printf("GET %q", req.URL)
		p.dlogger.Printf("%s: %s", hUserAgentKey, req.Header.Get(hUserAgentKey))
		p.dlogger.Printf("%s: %s", hRange, req.Header.Get(hRange))
		p.dlogger.Printf("backoff sleep: %s", dur)

		select {
		case startTime := <-startTimer.C:
			defer func() {
				p.Elapsed += time.Since(startTime)
			}()
			if attempt > 0 {
				ctxTimeout += timeoutIncBy
				mg.flash(&message{msg: "retrying..."})
				atomic.StoreInt32(&p.curTry, int32(attempt))
			} else {
				bar.AdjustAverageDecorators(startTime)
			}
			p.dlogger.Printf("ctxTimeout: %s", time.Duration(ctxTimeout)*time.Second)
		case <-ctx.Done():
			startTimer.Stop()
			return false, ctx.Err()
		}
		cctx, cancel := context.WithCancel(ctx)
		defer cancel()
		timer := time.AfterFunc(time.Duration(ctxTimeout)*time.Second, func() {
			cancel()
			msg := "timeout..."
			mg.flash(&message{msg: msg})
			p.dlogger.Print(msg)
		})
		defer timer.Stop()

		client := &http.Client{Transport: p.transport}
		resp, err := client.Do(req.WithContext(cctx))
		if err != nil {
			p.dlogger.Printf("client do: %s", err.Error())
			return ctx.Err() == nil, err
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

		if p.Written > 0 {
			p.dlogger.Printf("bar refill written: %d", p.Written)
			bar.SetRefill(p.Written)
			if p.Written-initialWritten == 0 {
				bar.IncrBy(int(p.Written), p.Elapsed)
				bar.AdjustAverageDecorators(time.Now().Add(-p.Elapsed))
			}
		}

		body := resp.Body
		if !p.quiet {
			body = bar.ProxyReader(resp.Body)
		}
		if body == nil {
			return false, ErrNilBody
		}
		defer body.Close()

		pWrittenSnap := p.Written
		buf, max := bytes.NewBuffer(make([]byte, 0, bufSize)), int64(bufSize)
		var n int64
		for timer.Reset(time.Duration(ctxTimeout) * time.Second) {
			n, err = io.CopyN(buf, body, max)
			if err != nil {
				p.dlogger.Printf("CopyN err: %s", err.Error())
				if ue, ok := err.(*url.Error); ok {
					mg.flash(&message{
						msg: fmt.Sprintf("%.28s...", ue.Err.Error()),
					})
					if ue.Temporary() {
						max -= n
						continue
					}
				}
				break
			}
			n, _ = io.Copy(fpart, buf)
			p.Written += n
			if total <= 0 {
				bar.SetTotal(p.Written+max*2, false)
			}
			max = bufSize
		}

		n, _ = io.Copy(fpart, buf)
		p.Written += n
		p.dlogger.Printf("total written: %d", p.Written-pWrittenSnap)
		if total <= 0 {
			p.Stop = p.Written - 1
			bar.SetTotal(p.Written, err == io.EOF)
		}
		if p.isDone() || ctx.Err() != nil {
			return false, ctx.Err()
		}
		// retry
		return true, err
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

func try(fn func(int) (bool, error)) error {
	var err error
	var cont bool
	var attempt int
	for {
		cont, err = fn(attempt)
		if !cont || err == nil {
			break
		}
		attempt++
	}
	return err
}

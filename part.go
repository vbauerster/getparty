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
	"time"

	"github.com/VividCortex/ewma"
	"github.com/pkg/errors"
	"github.com/vbauerster/backoff"
	"github.com/vbauerster/mpb/v4"
	"github.com/vbauerster/mpb/v4/decor"
)

const (
	bufSize      = 1 << 12
	maxTry       = 10
	timeoutIncBy = 5
)

var (
	ErrGiveUp  = fmt.Errorf("give up after %d tries", maxTry)
	ErrNilBody = errors.New("nil body")
)

// Part represents state of each download part
type Part struct {
	FileName string
	Start    int64
	Stop     int64
	Written  int64
	Elapsed  time.Duration
	Skip     bool

	order     int
	name      string
	dlogger   *log.Logger
	bg        *barGate
	transport *http.Transport
}

type barGate struct {
	bar     *mpb.Bar
	tryGate tryGate
	msgGate msgGate
}

func (s *barGate) init(progress *mpb.Progress, name string, order int, total int64) *barGate {

	if s == nil {
		s = &barGate{
			tryGate: tryGate{
				msgCh: make(chan string, 1),
				done:  make(chan struct{}),
			},
			msgGate: msgGate{
				msgCh: make(chan *message, 1),
				done:  make(chan struct{}),
			},
		}
		etaAge := math.Abs(float64(total))
		if total > bufSize {
			etaAge = float64(total) / float64(bufSize)
		}
		s.bar = progress.AddBar(total, mpb.BarStyle("[=>-|"),
			mpb.BarPriority(order),
			mpb.PrependDecorators(
				decor.Name(name+":", decor.WCSyncWidth),
				newTriesDecorator(s.tryGate, decor.WCSyncWidth),
				newPercentageWithTotal("%.1f%% of % .1f", s.msgGate, decor.WCSyncSpace),
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
	}

	return s
}

func (s *barGate) setTryMessage(msg string) {
	select {
	case s.tryGate.msgCh <- msg:
	case <-s.tryGate.done:
	}
}

func (s *barGate) flashMessage(msg *message) {
	msg.flashTimes = 15
	select {
	case s.msgGate.msgCh <- msg:
	case <-s.msgGate.done:
	}
}

func (p *Part) download(ctx context.Context, progress *mpb.Progress, req *http.Request, ctxTimeout uint) (err error) {

	defer func() {
		if err != nil {
			if !p.isDone() && p.bg != nil {
				p.bg.bar.Abort(false)
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

	initialWritten := p.Written
	prefixSnap := p.dlogger.Prefix()

	err = try(func(attempt int) (retry bool, err error) {
		if attempt > maxTry {
			return false, ErrGiveUp
		}
		if p.isDone() {
			p.dlogger.Println("done in try, quitting...")
			return false, nil
		}

		p.dlogger.SetPrefix(fmt.Sprintf("%s[%02d] ", prefixSnap, attempt))
		writtenSnap := p.Written
		defer func() {
			p.dlogger.Printf("total written: %d", p.Written-writtenSnap)
		}()

		dur := bOff.Backoff(attempt + 1)
		start := time.After(dur)

		total := p.Stop - p.Start + 1
		p.bg = p.bg.init(progress, p.name, p.order, total)
		req.Header.Set(hRange, p.getRange())
		p.dlogger.Printf("GET %q", req.URL)
		p.dlogger.Printf("%s: %s", hUserAgentKey, req.Header.Get(hUserAgentKey))
		p.dlogger.Printf("%s: %s", hRange, req.Header.Get(hRange))
		p.dlogger.Printf("backoff sleep: %s", dur)

		select {
		case <-start:
			if attempt > 0 {
				ctxTimeout += timeoutIncBy
				p.bg.flashMessage(&message{
					msg: "retrying...",
				})
				p.bg.setTryMessage(fmt.Sprintf("r#%02d:", attempt))
			}
			p.dlogger.Print("ctxTimeout: %s", time.Duration(ctxTimeout)*time.Second)
		case <-ctx.Done():
			return false, ctx.Err()
		}
		cctx, cancel := context.WithCancel(ctx)
		timer := time.AfterFunc(time.Duration(ctxTimeout)*time.Second, func() {
			cancel()
			msg := "timeout..."
			p.bg.flashMessage(&message{
				msg: msg,
			})
			p.dlogger.Print(msg)
		})
		defer cancel()

		client := &http.Client{Transport: p.transport}
		resp, err := client.Do(req.WithContext(cctx))
		if err != nil {
			p.dlogger.Printf("client do: %s", err.Error())
			return ctx.Err() == nil, err
		}

		startTime := time.Now()
		defer func() {
			p.Elapsed += time.Since(startTime)
		}()

		p.dlogger.Printf("resp.Status: %s", resp.Status)
		p.dlogger.Printf("resp.ContentLength: %d", resp.ContentLength)

		if resp.StatusCode == http.StatusOK {
			// no partial content, so download with single part
			if p.order > 0 {
				p.Skip = true
				p.bg.bar.Abort(true)
				p.dlogger.Print("no partial content, skipping...")
				return false, nil
			}
			total = resp.ContentLength
			p.bg.bar.SetTotal(total, false)
			p.dlogger.Printf("resetting written: %d", p.Written)
			p.Written = 0
		} else if resp.StatusCode != http.StatusPartialContent {
			return false, errors.Errorf("unexpected status: %s", resp.Status)
		}

		if p.Written > 0 {
			p.dlogger.Printf("bar refill written: %d", p.Written)
			p.bg.bar.SetRefill(p.Written)
			if p.Written-initialWritten == 0 {
				p.bg.bar.IncrBy(int(p.Written), p.Elapsed)
			}
		}

		max := int64(bufSize)
		buf := bytes.NewBuffer(make([]byte, 0, bufSize))
		body := p.bg.bar.ProxyReader(resp.Body)
		if body == nil {
			return false, ErrNilBody
		}
		defer body.Close()

		var written int64
		for timer.Reset(time.Duration(ctxTimeout) * time.Second) {
			written, err = io.CopyN(buf, body, max)
			if err != nil {
				p.dlogger.Printf("CopyN err: %s", err.Error())
				if ue, ok := err.(*url.Error); ok {
					p.bg.flashMessage(&message{
						msg: fmt.Sprintf("%.28s...", ue.Err.Error()),
					})
					if ue.Temporary() {
						max -= written
						continue
					}
				}
				timer.Stop()
				break
			}
			written, _ = io.Copy(fpart, buf)
			p.Written += written
			if total <= 0 {
				p.bg.bar.SetTotal(p.Written+max*2, false)
			}
			max = bufSize
		}

		written, _ = io.Copy(fpart, buf)
		p.Written += written
		if total <= 0 {
			p.Stop = p.Written - 1
			p.bg.bar.SetTotal(p.Written, err == io.EOF)
		}
		if err == io.EOF || ctx.Err() != nil {
			return false, ctx.Err()
		}
		// retry
		return true, err
	})

	if err == ErrGiveUp {
		done := make(chan struct{})
		p.bg.flashMessage(&message{
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
	return p.Skip || p.Stop-p.Start == p.Written-1
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

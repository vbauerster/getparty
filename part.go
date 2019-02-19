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
	"sync"
	"time"

	"github.com/VividCortex/ewma"
	"github.com/pkg/errors"
	"github.com/vbauerster/backoff"
	"github.com/vbauerster/mpb/v4"
	"github.com/vbauerster/mpb/v4/decor"
)

const (
	bufSize    = 1 << 12
	maxTimeout = 120
)

var ErrGiveUp = errors.New("give up")

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
	mu        sync.Mutex
	transport *http.Transport
}

type barGate struct {
	bar   *mpb.Bar
	msgCh chan *message
}

func (s *barGate) init(progress *mpb.Progress, name string, order int, total int64) *barGate {

	if s == nil {
		s = new(barGate)
		s.msgCh = make(chan *message)
		etaAge := math.Abs(float64(total))
		if total > bufSize {
			etaAge = float64(total) / float64(bufSize)
		}
		s.bar = progress.AddBar(total, mpb.BarStyle("[=>-|"),
			mpb.BarPriority(order),
			mpb.PrependDecorators(
				decor.Name(name+":"),
				percentageWithTotal("%.1f%% of % .1f", decor.WCSyncSpace, s.msgCh),
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

func (s *barGate) message(msg *message) {
	s.msgCh <- msg
}

func (p *Part) download(ctx context.Context, progress *mpb.Progress, req *http.Request, ctxTimeout uint) (err error) {
	if p.isDone() {
		return
	}

	defer func() {
		// just add method name, without stack trace at the point
		err = errors.WithMessage(err, "download: "+p.name)
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
		backoff.WithBaseDelay(20*time.Millisecond),
		backoff.WithResetDelay(2*time.Minute),
	)

	prefixSnap := p.dlogger.Prefix()
	err = try(func(attempt int) (retry bool, err error) {
		p.dlogger.SetPrefix(fmt.Sprintf("%s[%02d] ", prefixSnap, attempt))
		writtenSnap := p.Written
		defer func() {
			p.dlogger.Printf("total written: %d", p.Written-writtenSnap)
			if e := recover(); e != nil {
				p.dlogger.Printf("%#v", p)
				panic(e)
			}
		}()

		randomSleep := bOff.Backoff(attempt)
		p.dlogger.Printf("sleeping: %s", randomSleep)
		time.Sleep(randomSleep)

		req.Header.Set(hRange, p.getRange())
		p.dlogger.Printf("GET %q", req.URL)
		p.dlogger.Printf("%s: %s", hUserAgentKey, req.Header.Get(hUserAgentKey))
		p.dlogger.Printf("%s: %s", hRange, req.Header.Get(hRange))

		cctx, cancel := context.WithCancel(ctx)
		timeoutDur := time.Duration(ctxTimeout) * time.Second
		timeoutMsg := fmt.Sprintf("ctx timeout: %s", timeoutDur)
		timer := time.AfterFunc(timeoutDur, func() {
			cancel()
			p.bg.message(&message{
				msg:          timeoutMsg,
				displayTimes: 14,
			})
			p.dlogger.Print(timeoutMsg)
		})
		defer cancel()

		p.mu.Lock()
		tlsTimeout := p.transport.TLSHandshakeTimeout
		p.mu.Unlock()

		client := &http.Client{Transport: p.transport}
		resp, err := client.Do(req.WithContext(cctx))
		if err != nil {
			p.dlogger.Printf("client do: %v", err)
			if er, ok := err.(interface{ Timeout() bool }); ok && er.Timeout() {
				p.increaseTLSHandshakeTimeout(tlsTimeout)
			}
			if ctxTimeout >= maxTimeout {
				return false, ErrGiveUp
			}
			if cctx.Err() != nil {
				ctxTimeout += 5
			}
			return true, err
		}

		startTime := time.Now()
		defer func() {
			p.Elapsed += time.Since(startTime)
			if resp.Body != nil {
				if e := resp.Body.Close(); err == nil {
					err = e
				}
			}
		}()

		p.dlogger.Printf("resp.Status: %s", resp.Status)
		p.dlogger.Printf("resp.ContentLength: %d", resp.ContentLength)

		total := p.Stop - p.Start + 1
		if resp.StatusCode == http.StatusOK {
			// no partial content, so download with single part
			if p.order > 0 {
				p.Skip = true
				p.dlogger.Print("no partial content, skipping...")
				return false, nil
			}
			total = resp.ContentLength
			p.dlogger.Printf("reset last written: %d", p.Written)
			p.Written = 0
		} else if resp.StatusCode != http.StatusPartialContent {
			return false, errors.Errorf("unexpected status: %s", resp.Status)
		}

		p.bg = p.bg.init(progress, p.name, p.order, total)

		if p.Written > 0 {
			p.dlogger.Printf("bar refill written: %d", p.Written)
			p.bg.bar.SetRefill(p.Written)
			if attempt == 1 {
				p.bg.bar.IncrBy(int(p.Written), p.Elapsed)
			}
		}

		max := int64(bufSize)
		buf := bytes.NewBuffer(make([]byte, 0, bufSize))
		body := p.bg.bar.ProxyReader(resp.Body)

		var written int64
		for timer.Reset(time.Duration(ctxTimeout) * time.Second) {
			written, err = io.CopyN(buf, body, max)
			if err != nil {
				p.dlogger.Printf("CopyN err: %s", err.Error())
				if ue, ok := err.(*url.Error); ok {
					p.bg.message(&message{
						msg:          fmt.Sprintf("%.28s...", ue.Err.Error()),
						displayTimes: 14,
					})
					if ue.Temporary() {
						max -= written
						continue
					}
					if ue.Timeout() {
						p.increaseTLSHandshakeTimeout(tlsTimeout)
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
		if ctxTimeout >= maxTimeout {
			return false, ErrGiveUp
		}
		// retry
		ctxTimeout += 5
		p.bg.message(&message{
			msg:          fmt.Sprintf("retry: %02d", attempt),
			displayTimes: 14,
		})
		return true, err
	})
	if err == ErrGiveUp {
		done := make(chan struct{})
		p.bg.message(&message{
			msg:   err.Error(),
			final: true,
			done:  done,
		})
		<-done
	}
	return err
}

func (p *Part) increaseTLSHandshakeTimeout(tlsTimeout time.Duration) {
	p.mu.Lock()
	if p.transport.TLSHandshakeTimeout == tlsTimeout && tlsTimeout < 30*time.Second {
		p.transport.TLSHandshakeTimeout += 5 * time.Second
		p.dlogger.Printf("TLSHandshakeTimeout increase: %s", p.transport.TLSHandshakeTimeout)
	}
	p.mu.Unlock()
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
	attempt := 1
	for {
		cont, err = fn(attempt)
		if !cont || err == nil {
			break
		}
		attempt++
	}
	return err
}

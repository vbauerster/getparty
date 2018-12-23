package getparty

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"log"
	"math"
	"net/http"
	"os"
	"time"

	"github.com/VividCortex/ewma"
	"github.com/pkg/errors"
	"github.com/vbauerster/backoff"
	"github.com/vbauerster/mpb"
	"github.com/vbauerster/mpb/decor"
)

const (
	bufSize = 1 << 12
)

// Part represents state of each download part
type Part struct {
	FileName string
	Start    int64
	Stop     int64
	Written  int64
	Skip     bool

	order    int
	name     string
	client   *http.Client
	progress *mpb.Progress
	bar      *mpb.Bar
	dlogger  *log.Logger
}

func (p *Part) download(ctx context.Context, req *http.Request, timeout uint) (err error) {
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

	bOff := backoff.New(backoff.WithResetDelay(3 * time.Minute))
	messageCh := make(chan string, 1)
	if p.progress == nil {
		defer close(messageCh)
	}

	prefixSnap := p.dlogger.Prefix()
	return try(func(attempt int) (retry bool, err error) {
		p.dlogger.SetPrefix(fmt.Sprintf("%s[attempt#%02d] ", prefixSnap, attempt))
		writtenSnap := p.Written
		defer func() {
			p.dlogger.Printf("total written: %d", p.Written-writtenSnap)
			if e := recover(); e != nil {
				p.dlogger.Printf("%#v", p)
				panic(e)
			}
		}()

		req.Header.Set(hRange, p.getRange())
		p.dlogger.Printf("GET %q", req.URL)
		p.dlogger.Printf("%s: %s", hUserAgentKey, req.Header.Get(hUserAgentKey))
		p.dlogger.Printf("%s: %s", hRange, req.Header.Get(hRange))

		cctx, cancel := context.WithCancel(ctx)
		timer := time.AfterFunc(time.Duration(timeout)*time.Second, func() {
			p.dlogger.Printf("timeout: %s, canceling...", time.Duration(timeout)*time.Second)
			cancel()
		})
		defer cancel()

		resp, err := p.client.Do(req.WithContext(cctx))
		if err != nil {
			return false, err
		}

		body := resp.Body
		defer func() {
			if e := body.Close(); err == nil {
				err = e
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

		body = p.initBar(body, messageCh, attempt, total)

		if p.Written > 0 && p.bar != nil {
			p.dlogger.Printf("bar refill written: %d", p.Written)
			p.bar.SetRefill(int(p.Written), '+')
			if attempt == 1 {
				p.bar.IncrBy(int(p.Written))
			}
		}

		max := int64(bufSize)
		buf := bytes.NewBuffer(make([]byte, 0, bufSize))

		var written int64
		for timer.Reset(time.Duration(timeout) * time.Second) {
			written, err = io.CopyN(buf, body, max)
			if err != nil {
				p.dlogger.Printf("CopyN err: %v", err)
				// try to continue on temp err
				if e, ok := err.(interface{ Temporary() bool }); ok && e.Temporary() {
					max -= written
					messageCh <- "error: trying to continue"
					continue
				}
				timer.Stop()
				break
			}
			written, _ = io.Copy(fpart, buf)
			p.Written += written
			if total <= 0 && p.bar != nil {
				p.bar.SetTotal(p.Written+max*2, false)
			}
			max = bufSize
		}

		written, _ = io.Copy(fpart, buf)
		p.Written += written
		if total <= 0 && p.bar != nil {
			p.bar.SetTotal(p.Written, err == io.EOF)
		}
		if err == io.EOF || ctx.Err() != nil {
			return false, ctx.Err()
		}
		// full retry
		messageCh <- fmt.Sprintf("retry #%d", attempt)
		dur := bOff.Backoff(attempt)
		p.dlogger.Printf("sleep %s, before next attempt", dur)
		time.Sleep(dur)
		return true, err
	})
}

func (p *Part) initBar(body io.ReadCloser, msgCh <-chan string, attempt int, total int64) io.ReadCloser {
	if p.progress == nil {
		if attempt == 1 {
			go func() {
				for range msgCh {
				}
			}()
		}
		return body
	}

	if p.bar == nil {
		etaAge := math.Abs(float64(total))
		if total > bufSize {
			etaAge = float64(total) / float64(bufSize)
		}
		p.bar = p.progress.AddBar(total, mpb.BarPriority(p.order),
			mpb.PrependDecorators(
				decor.Name(p.name+":"),
				percentageWithTotal("%.1f%% of % .1f", decor.WCSyncSpace, msgCh, 12),
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

	return p.bar.ProxyReader(body).(io.ReadCloser)
}

func (p Part) getRange() string {
	if p.Stop <= 0 {
		return "bytes=0-"
	}
	return fmt.Sprintf("bytes=%d-%d", p.Start+p.Written, p.Stop)
}

func (p Part) isDone() bool {
	return p.Stop-p.Start == p.Written-1
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

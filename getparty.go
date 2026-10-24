//go:generate go tool stringer -type=sessionState

package getparty

import (
	"cmp"
	"context"
	"crypto/tls"
	"crypto/x509"
	"encoding/json"
	"errors"
	"expvar"
	"fmt"
	"io"
	"log"
	"maps"
	"math"
	"net/http"
	"net/http/cookiejar"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"runtime"
	"slices"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	flags "github.com/jessevdk/go-flags"
	"github.com/vbauerster/backoff"
	"github.com/vbauerster/backoff/exponential"
	"github.com/vbauerster/mpb/v8"
	"golang.org/x/net/publicsuffix"
	"golang.org/x/term"
)

type sessionState int

const (
	sessionUncompleted sessionState = iota
	sessionUncompletedWithAdvance
	sessionCompletedWithError
	sessionCompleted
)

const (
	cmdName     = "getparty"
	projectHome = "https://github.com/vbauerster/getparty"

	maxTimeout          = 90
	minFragment         = 512
	refreshRate         = 200
	hUserAgentKey       = "User-Agent"
	hContentDisposition = "Content-Disposition"
	hAcceptRanges       = "Accept-Ranges"
	hContentType        = "Content-Type"
	hRange              = "Range"
	hCookie             = "Cookie"
	hHost               = "Host"
	expCurrent          = "current"
	expTotal            = "total"
	expError            = "error"
)

var httpClient *http.Client // gets initialized at (*Cmd).getState
var expProgress *expvar.Map
var reContentDisposition *regexp.Regexp
var userAgents map[string]string

func init() {
	expProgress = expvar.NewMap("progress")
	reContentDisposition = regexp.MustCompile(`filename[^;\n=]*=(['"](.*?)['"]|[^;\n]*)`) // https://regex101.com/r/N4AovD/3
	userAgents = map[string]string{
		"chrome":  "Mozilla/5.0 (X11; Linux x86_64) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/155.0.0.0 Safari/537.36",
		"edge":    "Mozilla/5.0 (Macintosh; Intel Mac OS X 11_4) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/91.0.4472.101 Safari/537.36 Edg/91.0.864.37",
		"firefox": "Mozilla/5.0 (X11; Linux x86_64; rv:158.0) Gecko/20100101 Firefox/158.0",
		"safari":  "Mozilla/5.0 (Macintosh; Intel Mac OS X 11_4) AppleWebKit/605.1.15 (KHTML, like Gecko) Version/14.1 Safari/605.1.15",
	}
}

// options struct, represents cmd line options
type options struct {
	Parts       uint              `short:"p" long:"parts" value-name:"n" default:"1" description:"number of parts"`
	MaxRetry    uint              `short:"r" long:"max-retry" value-name:"n" default:"10" description:"max retries per each part, 0 for infinite"`
	MaxFollow   uint              `long:"max-follow" value-name:"n" default:"10" description:"max redirections to follow, 0 for infinite"`
	Timeout     uint              `short:"t" long:"timeout" value-name:"sec" default:"5" description:"initial timeout to fill a buffer"`
	BufferSize  uint              `short:"b" long:"buf-size" value-name:"KiB" choice:"2" choice:"4" choice:"8" choice:"16" default:"16" description:"buffer size per each part, prefer smaller for slow connection"`
	SpeedLimit  uint              `short:"l" long:"speed-limit" choice:"1" choice:"2" choice:"3" choice:"4" choice:"5" description:"speed limit (default: 0 = no limit; 5 = max limit)"`
	SessionName string            `short:"s" long:"session" value-name:"FILE" description:"json file of previous incomplete download session"`
	UserAgent   string            `short:"U" long:"user-agent" choice:"chrome" choice:"firefox" choice:"safari" choice:"edge" description:"User-Agent header (default: getparty/ver)"`
	AuthUser    string            `long:"username" description:"basic http auth username"`
	AuthPass    string            `long:"password" description:"basic http auth password"`
	Headers     map[string]string `short:"H" long:"header" value-name:"key:value" description:"http header, can be specified more than once"`
	Proxy       string            `short:"x" long:"proxy" value-name:"<[scheme://]host[:port]>" description:"proxy to use, if scheme is empty http is assumed"`
	Quiet       bool              `short:"q" long:"quiet" description:"quiet mode, progress bars are disabled"`
	Debug       bool              `short:"d" long:"debug" description:"debug mode, output debug info to stderr"`
	Version     bool              `short:"v" long:"version" description:"show version"`
	Https       struct {
		CertsFileName      string `short:"c" long:"certs-file" value-name:"certs.crt" description:"root certificates to use when verifying server certificates"`
		InsecureSkipVerify bool   `long:"no-check-cert" description:"don't verify the server's certificate chain and host name"`
	} `group:"Https Options"`
	Output struct {
		Name      string `short:"o" long:"name" value-name:"FILE" description:"output file name"`
		Overwrite bool   `short:"f" long:"overwrite" description:"overwrite existing file silently"`
		PathFirst bool   `long:"use-path" description:"resolve name from url path first (default: Content-Disposition header)"`
	} `group:"Output Options" namespace:"output"`
	BestMirror struct {
		Mirrors string `short:"m" long:"list" value-name:"FILE|-" description:"mirror list input"`
		Method  string `long:"method" choice:"HEAD" choice:"GET" default:"HEAD" description:"http method to use"`
		Doget   uint   `long:"doget" value-name:"KiB" description:"get first n KiB, implies method=GET"`
		Workers uint   `short:"w" long:"workers" value-name:"n" description:"max concurrent workers (default: number of logical CPUs)"`
		TopN    uint   `long:"top" value-name:"n" default:"1" description:"list top n mirrors without downloading unless n=1"`
		Pass    uint   `long:"pass" value-name:"n" default:"1" description:"query each mirror n times to get average result"`
		Timeout uint   `long:"timeout" value-name:"sec" default:"5" description:"query timeout"`
	} `group:"Best-mirror Options" namespace:"mirror"`
	Expose struct {
		Host string `long:"host" value-name:"host" description:"host (optional)"`
		Port uint   `long:"port" value-name:"port" description:"port (required)"`
	} `group:"Expose progress (host:port/debug/vars)" namespace:"expose"`
	Positional struct {
		Location string `positional-arg-name:"<url>" description:"http location"`
	} `positional-args:"yes"`
}

type httpRequestPatcher interface {
	patch(*http.Request)
}

// requestPatcher type used to patch *http.Request with headers
type requestPatcher struct {
	userinfo *url.Userinfo
	headers  map[string]string
}

func (p requestPatcher) patch(req *http.Request) {
	if req == nil {
		return
	}
	if p.userinfo != nil && req.URL != nil {
		req.URL.User = p.userinfo
	}
	for k, v := range p.headers {
		if k == hCookie {
			continue
		}
		switch k {
		case hHost:
			req.Host = v
		default:
			req.Header.Set(k, v)
		}
	}
}

func (p *requestPatcher) setHeaders(headers map[string]string) {
	p.headers = headers
}

// setUserAgent invariant: p.headers != nil
func (p *requestPatcher) setUserAgent(agent string) {
	p.headers[hUserAgentKey] = agent
}

// Cmd type used to manage download session
type Cmd struct {
	Ctx     context.Context
	Out     io.Writer
	Err     io.Writer
	opt     *options
	loggers [lEVELS]*log.Logger
}

func (m *Cmd) Exit(err error) (status int) {
	if err == nil {
		return 0
	}

	if m.loggers[DBUG] == nil {
		m.initLoggers()
	}

	defer func() {
		switch status {
		case 0, 2, 4:
			return
		}
		if m.opt != nil && m.opt.Debug {
			m.loggers[DBUG].Printf("ERROR: %s", err.Error())
			if e, ok := errors.AsType[*debugError](err); ok {
				_, err := m.Err.Write(e.stack)
				if err != nil {
					panic(err)
				}
			}
		}
	}()

	if e, ok := errors.AsType[*flags.Error](err); ok {
		if e.Type == flags.ErrHelp {
			return 0 // cmd invoked with --help switch
		}
		_, err := fmt.Fprintf(os.Stderr, "%s: try '%[1]s --help' for more information\n", cmdName)
		if err != nil {
			panic(err)
		}
		return 2
	}

	if e, ok := errors.AsType[ExpectedError](err); ok {
		if e == ErrInteractionRequired {
			_, err := fmt.Fprintln(os.Stderr, err.Error())
			if err != nil {
				panic(err)
			}
			return 4
		}
		if e == ErrMirrorStop {
			m.loggers[INFO].Println(err.Error())
			return 0
		}
		m.loggers[ERRO].Println(err.Error())
		return 1
	}

	m.loggers[ERRO].Println(err.Error())
	return 3
}

func (m *Cmd) Run(args []string, version, commit string) (err error) {
	defer func() {
		err = withMessage(err, "run")
	}()

	err = m.init(args)
	if err != nil {
		return err
	}

	userAgents[""] = fmt.Sprintf("%s/%s", cmdName, version)

	if m.opt.Version {
		_, e1 := fmt.Fprintf(os.Stdout, "%s (%.7s) (%s)\n", userAgents[""], commit, runtime.Version())
		_, e2 := fmt.Fprintf(os.Stdout, "Project home: %s\n", projectHome)
		return cmp.Or(e1, e2)
	}

	m.initLoggers()

	patcher, err := m.newRequestPatcher()
	if err != nil {
		return err
	}

	session, err := m.getState(patcher)
	if err != nil {
		if session != nil && errors.Is(err, ErrZeroParts) {
			session.summary("", m.loggers)
		}
		return cmp.Or(context.Cause(m.Ctx), err)
	}

	uncompletedParts, current := session.uncompletedPartsCount(), session.totalWritten()
	progress := newProgress(m.Ctx, m.Out, m.Err, uncompletedParts)
	stateQuery := makeStateQuery(current, session.ContentLength, session.isResumable())
	outputName := filepath.Join(session.dir, session.OutputName)
	ctx, cancel := context.WithCancelCause(m.Ctx)
	session.summary(outputName, m.loggers)
	defer func() {
		cancel(nil)
		tw := session.totalWritten()
		state := stateQuery(tw, err)
		m.loggers[DBUG].Println(state, tw)
		switch state {
		case sessionUncompletedWithAdvance, sessionCompletedWithError:
			dumpName := outputName
			if errors.Is(context.Cause(ctx), errSessionCanceled) {
				dumpName += ".recovered"
			} else {
				dumpName += ".json"
			}
			defer func() {
				if x := recover(); x != nil {
					_ = json.NewEncoder(os.Stderr).Encode(session)
					panic(x)
				} else {
					m.loggers[INFO].Printf("Session state saved; to resume run: %s --session %q", cmdName, dumpName)
				}
			}()
			if err := session.dumpState(dumpName); err != nil {
				// defer panic to fallthrough take place
				defer panic(err)
			}
			fallthrough
		case sessionUncompleted:
			for _, p := range session.Parts {
				if p.Written == 0 {
					stat, err := p.output.Stat()
					if err != nil {
						m.loggers[DBUG].Printf("%q stat: %s", p.output, err.Error())
						continue
					}
					if stat.Size() == 0 {
						m.loggers[DBUG].Printf("%q is zero: close,remove: %v", p.output, cmp.Or(p.output.Close(), os.Remove(p.output.Name())))
					}
				} else {
					m.loggers[DBUG].Printf("%q close: %v", p.output, p.output.Close())
				}
			}
			if p := session.Parts[0]; session.Single && !session.isResumable() && p.output != nil {
				m.loggers[DBUG].Printf("%q rename to %q: %v", p.output, outputName, os.Rename(p.output.Name(), outputName))
			}
			progress.Wait()
		case sessionCompleted:
			of := session.Parts[0].output
			err = cmp.Or(of.Sync(), of.Close(), os.Rename(of.Name(), outputName))
			m.loggers[DBUG].Printf("%q sync,close,rename to %q: %v", of, outputName, err)
			if m.opt.SessionName != "" && err == nil {
				m.loggers[DBUG].Printf("%q remove: %v", m.opt.SessionName, os.Remove(m.opt.SessionName))
			}
			progress.Wait()
			m.loggers[INFO].Printf("%q saved [%d/%d]", outputName, session.ContentLength, tw)
		}
	}()

	_ = expProgress.Init()
	expProgress.Add(expTotal, session.ContentLength)
	expProgress.Add(expCurrent, current)
	expProgress.Set(expError, nil)
	if ok, shutdown := m.expListenAndServe(); ok {
		defer shutdown()
	}

	firstResp := newFirstHttpResponseContext(m.Ctx)
	options := downloadOptions{
		maxTry:  m.opt.MaxRetry,
		timeout: m.getTimeout(),
		sleep:   time.Duration(m.opt.SpeedLimit*50) * time.Millisecond,
	}
	m.loggers[DBUG].Printf("Common options: maxTry=%d timeout=%s sleep=%s", options.maxTry, options.timeout, options.sleep)

	var i uint
	var chunks [][]byte
	{
		size := m.opt.BufferSize * 1024
		row := make([]byte, size*uint(uncompletedParts))
		for range uncompletedParts {
			chunks = append(chunks, slices.Clip(row[i:i+size]))
			i += size
		}
		i = 0
	}
	var doneCount atomic.Uint32
	var wg sync.WaitGroup
	errCh := make(chan error, 1)

	for _, p := range session.Parts {
		if err := p.init(session); err != nil {
			cancel(errSessionCanceled)
			wg.Wait()
			return err
		}
		if p.isContentDownloaded() {
			doneCount.Add(1)
			continue
		}
		p.ctx = ctx
		p.patcher = patcher
		p.firstResp = firstResp
		p.progress = progress
		p.logger = log.New(m.Err, fmt.Sprintf(prefixFormat, p.name, 0), log.LstdFlags)
		buf := chunks[i]
		i++
		wg.Add(1) // prefer wg.Add to wg.Go because wg.Done must be called on panic
		go func() {
			var err error
			defer func() {
				if x := recover(); x != nil {
					if e, ok := x.(error); ok {
						err = fmt.Errorf("%s recovered: %w", p.name, e)
					} else {
						err = fmt.Errorf("%s recovered: %v", p.name, x)
					}
					cancel(errSessionCanceled)
				}
				if err != nil {
					select {
					case errCh <- err:
					default:
					}
				} else if p.isDone() {
					// p.isDone instead of p.isContentDownloaded to allow zero ContentLength to complete
					doneCount.Add(1)
				}
				wg.Done()
			}()
			err = p.download(session.location, options, buf)
		}()
	}

	go func() {
		wg.Wait()
		firstResp.cancel(nil)
	}()

	<-firstResp.ctx.Done()

	start := time.Now()
	cause := context.Cause(firstResp.ctx)
	m.loggers[DBUG].Printf("%T: %v", firstResp, cause)

	switch {
	case errors.Is(cause, errContextFallback):
		if session.restored {
			cancel(errSessionCanceled)
			wg.Wait()
			panic(fmt.Errorf(
				"restored session is expected to have status %d but it is %d instead",
				http.StatusPartialContent,
				http.StatusOK,
			))
		}
		if !session.Single {
			session.Single = true
			wg.Wait()
			id := <-firstResp.id
			var part *Part
			for _, p := range session.Parts {
				if p.Id == id {
					part = p
				} else {
					m.loggers[DBUG].Printf("%q close: %v", p.output, p.output.Close())
				}
			}
			if part == nil {
				cancel(errSessionCanceled)
				panic(fmt.Errorf("unexpected firstResp id: %d", id))
			}
			session.Parts[0] = part
			session.Parts = slices.Clip(session.Parts[:1])
		}
	case errors.Is(cause, errContextPartial):
		if !session.Single {
			bar, err := progress.addTotalBar(
				start.Add(-session.Elapsed),
				session.ContentLength,
				len(session.Parts),
				&doneCount,
			)
			if err != nil {
				return withStack(err)
			}
			if current != 0 {
				m.loggers[DBUG].Println("Setting total current:", current)
				bar.SetCurrent(current)
				bar.SetRefillCurrent()
			}
			var wg sync.WaitGroup
			defer func() {
				close(progress.totalCh)
				wg.Wait()
				bar.Abort(false)
			}()
			incr := func(n int64) {
				expProgress.Add(expCurrent, n)
				bar.IncrInt64(n)
			}
			for range max(cap(progress.totalCh)/3, 1) {
				wg.Go(func() {
					var stash int64
					for n := range progress.totalCh {
						if stash == 0 {
							stash = n
						} else {
							incr(stash + n)
							stash = 0
						}
					}
					if stash != 0 {
						incr(stash)
					}
				})
			}
		}
	}

	wg.Wait()
	session.Elapsed += time.Since(start)

	close(errCh)
	if v, ok := <-errCh; ok {
		err = v
	} else {
		err = context.Cause(m.Ctx)
	}

	if err != nil {
		expProgress.Set(expError, expvar.Func(func() any {
			// returning just err will result in empty object like '"error":{}'
			// therefore call Error() instead to have some error string value
			return err.Error()
		}))
		return err
	}

	if doneCount.Load() != uint32(len(session.Parts)) {
		cancel(errSessionCanceled)
		panic(errors.New("done count != session parts"))
	}

	return withStack(session.merge(progress, m.loggers[DBUG]))
}

func (m *Cmd) init(args []string) error {
	if m.Ctx == nil {
		m.Ctx = context.Background()
	}
	if m.Out == nil {
		m.Out = os.Stdout
	}
	if m.Err == nil {
		m.Err = os.Stderr
	}
	m.opt = new(options)
	_, err := flags.NewParser(m.opt, flags.Default).ParseArgs(args)
	return err
}

func (m *Cmd) getState(patcher *requestPatcher) (session *Session, err error) {
	defer func() {
		err = withMessage(err, "getState")
	}()
	rtBuilder := newRoundTripperBuilder(m.loggers[DBUG])
	if m.opt.Proxy != "" {
		fixedURL, err := url.Parse(m.opt.Proxy)
		if err != nil {
			return nil, withStack(err)
		}
		rtBuilder = rtBuilder.proxy(fixedURL)
	}
	tlsConfig, err := m.getTLSConfig()
	if err != nil {
		return nil, withStack(err)
	}
	rtBuilder = rtBuilder.tls(tlsConfig)
	jar, err := cookiejar.New(&cookiejar.Options{
		PublicSuffixList: publicsuffix.List, // All users of cookiejar should import "golang.org/x/net/publicsuffix"
	})
	if err != nil {
		return nil, withStack(err)
	}
	patcher.setHeaders(m.opt.Headers)
	patcher.setUserAgent(userAgents[m.opt.UserAgent])
	for {
		switch {
		case m.opt.SessionName != "":
			restored := new(Session)
			if err := restored.loadState(m.opt.SessionName); err != nil {
				return nil, withStack(err)
			}
			m.loggers[DBUG].Printf("Session restored from: %q", m.opt.SessionName)
			patcher.setHeaders(restored.Headers)
			if m.opt.UserAgent != "" {
				// respect UserAgent explicitly set by user
				patcher.setUserAgent(userAgents[m.opt.UserAgent])
			}
			if httpClient == nil {
				httpClient = &http.Client{
					Jar:       jar,
					Transport: rtBuilder.pool(true).build(),
				}
			}
			if session != nil {
				if restored.URL != session.URL {
					m.loggers[DBUG].Printf("Updating session.URL from: %q to: %q", restored.URL, session.URL)
					restored.URL = session.URL
				}
				if maps.Equal(restored.Headers, session.Headers) {
					restored.location = session.location
					return restored, withStack(checkContentMismatch(restored, session))
				}
			}
			// re-follow with patcher set to restored.HeaderMap and restored.URL
			m.opt.Headers = restored.Headers
			m.opt.Output.Name = restored.OutputName
			session, err = m.follow(patcher, restored.URL)
			if err != nil {
				return nil, err
			}
			session.Single = restored.Single
			session.Parts = restored.Parts
			session.Elapsed = restored.Elapsed
			session.restored = true
			session.dir = filepath.Dir(m.opt.SessionName)
			return session, withStack(checkContentMismatch(restored, session))
		case m.opt.BestMirror.Mirrors != "":
			top, err := m.bestMirror(rtBuilder.pool(false).build(), patcher)
			if err != nil {
				return nil, withStack(err)
			}
			for _, mir := range top[:cmp.Or(min(m.opt.BestMirror.TopN, uint(len(top))), uint(len(top)))] {
				m.loggers[INFO].Println(mir.dur.Truncate(time.Microsecond), mir.url)
			}
			if m.opt.BestMirror.TopN != 1 {
				return nil, withStack(ErrMirrorStop)
			}
			m.opt.Positional.Location = top[0].url
			fallthrough
		case m.opt.Positional.Location != "":
			httpClient = &http.Client{
				Jar:       jar,
				Transport: rtBuilder.pool(true).build(),
			}
			session, err = m.follow(patcher, m.opt.Positional.Location)
			if err != nil {
				return nil, err
			}
			state := session.OutputName + ".json"
			exist, err := isFileExist(state)
			if err != nil {
				return nil, withStack(err)
			}
			if exist {
				m.opt.SessionName = state
				break // goto case m.opt.SessionName != "":
			}
			err = session.makeParts(m.opt.Parts)
			if err != nil {
				return session, withStack(err)
			}
			exist, err = isFileExist(session.OutputName)
			if err != nil {
				return nil, withStack(err)
			}
			if exist {
				return session, withStack(m.replaceOutput(session.OutputName))
			}
			return session, nil
		default:
			return nil, new(flags.Error)
		}
	}
}

func (m Cmd) newRequestPatcher() (*requestPatcher, error) {
	patcher := new(requestPatcher)
	if m.opt.AuthUser != "" {
		if m.opt.AuthPass == "" {
			pass, err := m.readPassword()
			if err != nil {
				return nil, withStack(err)
			}
			m.opt.AuthPass = pass
		}
		patcher.userinfo = url.UserPassword(m.opt.AuthUser, m.opt.AuthPass)
		m.opt.AuthUser = ""
		m.opt.AuthPass = ""
	}
	return patcher, nil
}

func (m Cmd) getTLSConfig() (config *tls.Config, err error) {
	defer func() {
		err = withMessage(err, "getTLSConfig")
	}()
	config = new(tls.Config)
	if m.opt.Https.InsecureSkipVerify {
		config.InsecureSkipVerify = true
	}
	if m.opt.Https.CertsFileName != "" {
		buf, err := os.ReadFile(m.opt.Https.CertsFileName)
		if err != nil {
			return nil, err
		}
		pool, err := x509.SystemCertPool()
		if err != nil {
			return nil, err
		}
		if ok := pool.AppendCertsFromPEM(buf); !ok {
			return nil, fmt.Errorf("bad cert file %q", m.opt.Https.CertsFileName)
		}
		config.RootCAs = pool
	}
	return config, nil
}

// follow invariant: httpClient != nil
func (m Cmd) follow(patcher httpRequestPatcher, rawURL string) (session *Session, err error) {
	defer func() {
		httpClient.CheckRedirect = nil
		err = withMessage(err, "follow")
	}()

	httpClient.CheckRedirect = func(*http.Request, []*http.Request) error {
		return http.ErrUseLastResponse
	}

	var fc uint
	template := "R%02d:F%02d %s"
	location := rawURL
	timeout := m.getTimeout()
	maxRetry := cmp.Or(m.opt.MaxRetry, math.MaxUint)
	maxFollow := cmp.Or(m.opt.MaxFollow, math.MaxUint-1) + 1

	err = backoff.RetryWithContext(m.Ctx, exponential.New(exponential.WithBaseDelay(500*time.Millisecond)),
		func(attempt uint, _ func()) (retry bool, err error) {
			ctx, cancel := context.WithTimeoutCause(m.Ctx, timeout, errTimeout)
			defer func() {
				cancel()
				if errors.Is(context.Cause(ctx), errTimeout) {
					timeout = min(timeout+5*time.Second, maxTimeout*time.Second)
					m.loggers[WARN].Println(timeoutMsg, errTimeout.Error())
					m.loggers[DBUG].Println(timeoutMsg, errTimeout.Error())
				} else if err != nil {
					m.loggers[WARN].Println(unwrapOrErr(err).Error())
					m.loggers[DBUG].Println(err.Error())
				}
			}()
			for range maxFollow - fc {
				m.loggers[INFO].Printf(template, attempt, fc, location)
				m.loggers[DBUG].Printf(template, attempt, fc, location)

				req, err := http.NewRequest(http.MethodHead, location, nil)
				if err != nil {
					return false, withStack(err)
				}

				patcher.patch(req)

				for k, v := range req.Header {
					m.loggers[DBUG].Printf("Request Header: %s: %v", k, v)
				}

				resp, err := httpClient.Do(req.WithContext(ctx))
				if err != nil {
					if attempt == maxRetry {
						return false, withStack(ErrMaxRetry)
					}
					return true, withStack(err)
				}

				if jar := httpClient.Jar; jar != nil {
					for _, cookie := range jar.Cookies(req.URL) {
						m.loggers[DBUG].Println("Cookie:", cookie) // *http.Cookie implements fmt.Stringer
					}
				}

				m.loggers[DBUG].Println("Response Status:", resp.Status)

				for k, v := range resp.Header {
					m.loggers[DBUG].Printf("Response Header: %s: %v", k, v)
				}

				if isRedirect(resp.StatusCode) {
					m.loggers[INFO].Println("Response Status:", resp.Status)
					loc, err := resp.Location()
					if err != nil {
						return false, withStack(err)
					}
					location = loc.String()
					fc++
					continue
				}

				if resp.StatusCode != http.StatusOK {
					if isServerError(resp.StatusCode) { // server error may be temporary
						if attempt == maxRetry {
							return false, withStack(ErrMaxRetry)
						}
						return true, withStack(UnexpectedHttpStatusError(resp.StatusCode))
					}
					return false, withStack(UnexpectedHttpStatusError(resp.StatusCode))
				}

				m.loggers[INFO].Println("Response Status:", resp.Status)

				if m.opt.Output.Name == "" {
					for _, pf := range []bool{m.opt.Output.PathFirst, !m.opt.Output.PathFirst} {
						name, err := parseOutputName(location, resp.Header, pf)
						if err != nil {
							return false, withStack(err)
						}
						m.loggers[DBUG].Printf("PathFirst: %t; OutputName: %q", pf, name)
						if name != "" {
							m.opt.Output.Name = name
							break
						}
					}
				}

				session = &Session{
					location:      location,
					URL:           rawURL,
					OutputName:    cmp.Or(m.opt.Output.Name, "unknown"),
					AcceptRanges:  resp.Header.Get(hAcceptRanges),
					ContentType:   resp.Header.Get(hContentType),
					ContentLength: resp.ContentLength,
					Headers:       m.opt.Headers,
				}
				return false, withStack(resp.Body.Close())
			}
			return false, withStack(ErrMaxRedirect)
		})

	return session, err
}

func (m Cmd) readPassword() (string, error) {
	_, err := fmt.Fprint(m.Out, "Enter password: ")
	if err != nil {
		return "", err
	}
	pass, err := term.ReadPassword(int(os.Stdin.Fd()))
	if err != nil {
		return "", err
	}
	_, err = fmt.Fprintln(m.Out)
	if err != nil {
		return "", err
	}
	return string(pass), context.Cause(m.Ctx)
}

func (m Cmd) replaceOutput(name string) error {
	if m.opt.Output.Overwrite {
		err := os.Remove(name)
		m.loggers[DBUG].Printf("%q force remove: %v", name, err)
		return err
	}
	if m.opt.Quiet {
		return ErrInteractionRequired
	}
	m.loggers[WARN].Printf("Output file %q already exists, overwrite? [Y/n]", name)
	state, err := term.MakeRaw(int(os.Stdin.Fd()))
	if err != nil {
		return err
	}
	defer func() {
		err = cmp.Or(err, term.Restore(int(os.Stdin.Fd()), state))
	}()
	b := make([]byte, 1)
	_, err = os.Stdin.Read(b)
	if err != nil {
		return err
	}
	switch b[0] {
	case 'y', 'Y', '\r':
		err := os.Remove(name)
		m.loggers[DBUG].Printf("%q remove: %v", name, err)
		return err
	default:
		return ErrCanceledByUser
	}
}

func (m Cmd) getTimeout() time.Duration {
	timeout := min(cmp.Or(m.opt.Timeout, 5), maxTimeout)
	return time.Duration(timeout) * time.Second
}

func (m Cmd) getBestMirrorTimeout() time.Duration {
	timeout := min(cmp.Or(m.opt.BestMirror.Timeout, 5), maxTimeout)
	return time.Duration(timeout) * time.Second
}

func (m Cmd) expListenAndServe() (bool, func()) {
	if m.opt.Expose.Port == 0 {
		return false, nil
	}
	addr := m.opt.Expose.Host + ":" + strconv.FormatUint(uint64(m.opt.Expose.Port), 10)
	m.loggers[DBUG].Printf("Exposing progress at %s/debug/vars", addr)
	srv := &http.Server{Addr: addr}
	go func() {
		err := srv.ListenAndServe()
		if !errors.Is(err, http.ErrServerClosed) {
			panic(err)
		}
	}()
	return true, func() {
		err := srv.Shutdown(m.Ctx)
		m.loggers[DBUG].Printf("Expvar server shutdown: %v", err)
	}
}

// https://go.dev/play/p/Q25_gze66yB
func concat(logger *log.Logger, bar *mpb.Bar, parts []*outFile) error {
	if len(parts) < 2 {
		bar.Increment()
		return nil
	}

	var wg sync.WaitGroup
	errCh := make(chan error, 1)

	for c := range slices.Chunk(parts, 2) {
		if len(c) != 2 {
			break
		}
		dst, src := c[0], c[1]
		// dst.file can be nil when session is restored with some done part
		if dst.file == nil {
			err := dst.Open(os.O_WRONLY | os.O_APPEND)
			logger.Printf("dst=%q reopen: %v", dst, err)
			if err != nil {
				return err
			}
		}
		// have to reopen src.file for reading
		err := src.Open(os.O_RDONLY)
		logger.Printf("src=%q reopen: %v", src, err)
		if err != nil {
			return err
		}

		wg.Go(func() {
			n, err := io.Copy(dst.file, src.file)
			logger.Printf("dst=%q <- src=%q %d: %v", dst, src, n, err)
			if err != nil {
				select {
				case errCh <- err:
				default:
				}
				return
			}

			err = cmp.Or(src.Close(), os.Remove(src.Name()))
			logger.Printf("src=%q close,remove: %v", src, err)
			if err != nil {
				select {
				case errCh <- err:
				default:
				}
				return
			}

			bar.Increment()
			src.file = nil
		})
	}

	wg.Wait()
	close(errCh)

	if err := <-errCh; err != nil {
		for _, p := range parts {
			logger.Printf("%q close: %v", p, p.Close())
		}
		return err
	}

	i := 0
	for _, p := range parts {
		if p.file != nil {
			parts[i] = p
			i++
		}
	}

	return concat(logger, bar, parts[:i])
}

func parseOutputName(location string, header http.Header, pathFirst bool) (string, error) {
	if pathFirst {
		nURL, err := url.Parse(location)
		if err != nil {
			return "", err
		}
		var name string
		switch {
		case nURL.Path != "":
			name = nURL.Path
		case nURL.Opaque != "":
			name = nURL.Opaque
			fallthrough
		default:
			unescaped, err := url.QueryUnescape(name)
			if err != nil {
				return "", err
			}
			name = unescaped
		}
		if name != "" {
			return filepath.Base(name), nil
		}
	}
	return url.QueryUnescape(parseContentDisposition(header.Get(hContentDisposition)))
}

func parseContentDisposition(input string) string {
	group := reContentDisposition.FindStringSubmatch(input)
	if len(group) != 3 {
		return ""
	}
	if group[2] != "" {
		return group[2]
	}
	b, a, found := strings.Cut(group[1], "''")
	if found && strings.EqualFold(b, "utf-8") {
		return a
	}
	if b != `""` {
		return b
	}
	return ""
}

func isRedirect(status int) bool {
	return status > 299 && status < 400
}

func isServerError(status int) bool {
	return status > 499 && status < 600
}

func isFileExist(name string) (bool, error) {
	stat, err := os.Stat(name)
	if err == nil {
		if stat.IsDir() {
			return true, fmt.Errorf("%q is a directory", name)
		}
		return true, nil
	}
	if errors.Is(err, os.ErrNotExist) {
		return false, nil
	}
	return false, err
}

func checkContentMismatch(old, new *Session) error {
	if old.ContentType != new.ContentType {
		return ContentMismatchError[string]{
			kind: mismatchContentType,
			old:  old.ContentType,
			new:  new.ContentType,
		}
	}
	if old.ContentLength != new.ContentLength {
		return ContentMismatchError[int64]{
			kind: mismatchContentLength,
			old:  old.ContentLength,
			new:  new.ContentLength,
		}
	}
	return nil
}

func makeStateQuery(current, contentLength int64, resumable bool) func(int64, error) sessionState {
	if !resumable {
		return func(_ int64, err error) sessionState {
			if err != nil {
				return sessionUncompleted
			}
			return sessionCompleted
		}
	}
	return func(newCurrent int64, err error) sessionState {
		if newCurrent != contentLength {
			if newCurrent != current {
				// if some bytes were written
				return sessionUncompletedWithAdvance
			}
			return sessionUncompleted
		}
		if err != nil {
			return sessionCompletedWithError
		}
		return sessionCompleted
	}
}

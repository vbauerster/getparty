//go:generate go tool stringer -type=sessionMode -trimprefix=mode

package getparty

import (
	"cmp"
	"context"
	"crypto/tls"
	"crypto/x509"
	"errors"
	"fmt"
	"io"
	"log"
	"math"
	"net/http"
	"net/http/cookiejar"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"runtime"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	flags "github.com/jessevdk/go-flags"
	"github.com/vbauerster/backoff"
	"github.com/vbauerster/backoff/exponential"
	"github.com/vbauerster/mpb/v8"
	"golang.org/x/net/publicsuffix"
	"golang.org/x/sync/errgroup"
	"golang.org/x/term"
)

type sessionState int
type sessionMode int

func (s sessionMode) Error() string {
	return s.String()
}

const (
	sessionUncompleted sessionState = iota
	sessionUncompletedWithAdvance
	sessionCompletedWithError
	sessionCompleted
)

const (
	modePartial sessionMode = iota
	modeFallback
)

const (
	ErrCanceledByUser      = ExpectedError("Canceled by user")
	ErrMaxRedirect         = ExpectedError("Max redirections")
	ErrMaxRetry            = ExpectedError("Max retries")
	ErrZeroParts           = ExpectedError("No parts no work")
	ErrTooFragmented       = ExpectedError("Too fragmented, reduce number of parts maybe?")
	ErrInteractionRequired = ExpectedError("Interaction required, run without --queit maybe?")
)

const (
	cmdName     = "getparty"
	projectHome = "https://github.com/vbauerster/getparty"

	umask               = 0644
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
)

var httpClient *http.Client
var reContentDisposition = regexp.MustCompile(`filename[^;\n=]*=(['"](.*?)['"]|[^;\n]*)`) // https://regex101.com/r/N4AovD/3
var userAgents = map[string]string{
	"chrome":  "Mozilla/5.0 (X11; Linux x86_64) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/143.0.0.0 Safari/537.36",
	"edge":    "Mozilla/5.0 (Macintosh; Intel Mac OS X 11_4) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/91.0.4472.101 Safari/537.36 Edg/91.0.864.37",
	"firefox": "Mozilla/5.0 (X11; Linux x86_64; rv:146.0) Gecko/20100101 Firefox/146.0",
	"safari":  "Mozilla/5.0 (Macintosh; Intel Mac OS X 11_4) AppleWebKit/605.1.15 (KHTML, like Gecko) Version/14.1 Safari/605.1.15",
}

// options struct, represents cmd line options
type options struct {
	Parts       uint              `short:"p" long:"parts" value-name:"n" default:"1" description:"number of parts"`
	MaxRetry    uint              `short:"r" long:"max-retry" value-name:"n" default:"10" description:"max retries per each part, 0 for infinite"`
	MaxRedirect uint              `long:"max-redirect" value-name:"n" default:"10" description:"max redirections allowed, 0 for infinite"`
	Timeout     uint              `short:"t" long:"timeout" value-name:"sec" default:"10" description:"initial timeout to fill a buffer"`
	BufferSize  uint              `short:"b" long:"buf-size" value-name:"KiB" choice:"2" choice:"4" choice:"8" choice:"16" default:"8" description:"buffer size, prefer smaller for slow connection"`
	SpeedLimit  uint              `short:"l" long:"speed-limit" choice:"1" choice:"2" choice:"3" choice:"4" choice:"5" description:"speed limit gauge"`
	SessionName string            `short:"s" long:"session" value-name:"FILE" description:"session state of incomplete download, file with json extension"`
	UserAgent   string            `short:"U" long:"user-agent" choice:"chrome" choice:"firefox" choice:"safari" choice:"edge" description:"User-Agent header (default: getparty/ver)"`
	AuthUser    string            `long:"username" description:"basic http auth username"`
	AuthPass    string            `long:"password" description:"basic http auth password"`
	HeaderMap   map[string]string `short:"H" long:"header" value-name:"key:value" description:"http header, can be specified more than once"`
	Proxy       string            `short:"x" long:"proxy" value-name:"<[scheme://]host[:port]>" description:"use the specified proxy, if scheme is empty http is assumed"`
	Quiet       bool              `short:"q" long:"quiet" description:"quiet mode, no progress bars"`
	Debug       bool              `short:"d" long:"debug" description:"enable debug to stderr"`
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
		MaxGo   uint   `short:"g" long:"max" value-name:"n" description:"max concurrent http request (default: number of logical CPUs)"`
		TopN    uint   `long:"top" value-name:"n" default:"1" description:"list top n mirrors, download condition n=1"`
		Pass    uint   `long:"pass" value-name:"n" default:"1" description:"query each mirror n times to get average result"`
	} `group:"Best-mirror Options" namespace:"mirror"`
	Positional struct {
		Location string `positional-arg-name:"<url>" description:"http location"`
	} `positional-args:"yes"`
}

// helper type used for parts concatenation
type catFile struct {
	name string
	file *os.File
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

func (p *requestPatcher) setUserAgent(agent string) {
	p.headers[hUserAgentKey] = agent
}

// Cmd type used to manage download session
type Cmd struct {
	Ctx context.Context
	Out io.Writer
	Err io.Writer

	opt     *options
	loggers [lEVELS]*log.Logger
}

func (m *Cmd) Exit(err error) (status int) {
	if err == nil {
		return 0
	}

	defer func() {
		switch status {
		case 0, 2, 4:
			return
		}
		if m.opt != nil && m.opt.Debug {
			m.loggers[DBUG].Printf("ERROR: %s", err.Error())
			if e := (*debugError)(nil); errors.As(err, &e) {
				_, err := m.Err.Write(e.stack)
				if err != nil {
					panic(err)
				}
			}
		}
	}()

	if e := (*flags.Error)(nil); errors.As(err, &e) {
		if e.Type == flags.ErrHelp {
			return 0 // cmd invoked with --help switch
		}
		_, err := fmt.Fprintf(os.Stderr, "%s: try '%[1]s --help' for more information\n", cmdName)
		if err != nil {
			panic(err)
		}
		return 2
	}

	if e := ExpectedError(""); errors.As(err, &e) {
		if e == ErrInteractionRequired {
			log.Default().Println(err.Error())
			return 4
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

	m.init()
	parser := flags.NewParser(m.opt, flags.Default)
	_, err = parser.ParseArgs(args)
	if err != nil {
		return err
	}

	userAgents[""] = fmt.Sprintf("%s/%s", cmdName, version)

	if m.opt.Version {
		_, e1 := fmt.Fprintf(m.Out, "%s (%.7s) (%s)\n", userAgents[""], commit, runtime.Version())
		_, e2 := fmt.Fprintf(m.Out, "Project home: %s\n", projectHome)
		err := cmp.Or(e1, e2)
		if err != nil {
			m.initLoggers()
		}
		return err
	}

	m.initLoggers()

	patcher, err := m.newRequestPatcher()
	if err != nil {
		return err
	}

	session, err := m.getState(patcher)
	if err != nil {
		if session != nil && errors.Is(err, ErrZeroParts) {
			session.summary(m.loggers, false)
		}
		return err
	}
	session.summary(m.loggers, true)

	var recovered bool
	progress := newProgress(m.Ctx, session, m.Out, m.Err)
	stateQuery := makeStateQuery(session, progress.current)
	defer func() {
		var dump, completed bool
		tw := session.totalWritten()
		switch stateQuery(tw, err) {
		case sessionUncompletedWithAdvance, sessionCompletedWithError:
			dump = true
		case sessionCompleted:
			completed = true
			fallthrough
		case sessionUncompleted:
			progress.Wait()
		}
		switch {
		case dump:
			var dumpName string
			if recovered {
				dumpName = session.OutputName + ".recovered"
			} else {
				dumpName = session.OutputName + ".json"
			}
			err := session.dumpState(dumpName)
			m.loggers[DBUG].Printf("Session state dumped with: %v", err)
			progress.Wait()
			if err == nil {
				m.loggers[INFO].Printf("Session state saved to %q", dumpName)
			}
		case completed:
			m.loggers[INFO].Printf("%q saved [%d/%d]", session.OutputName, session.ContentLength, tw)
		}
	}()

	var doneCount uint32
	var eg errgroup.Group
	var recoverHandler sync.Once
	firstResp := &firstHttpResponseContext{id: make(chan int, 1)}
	firstResp.ctx, firstResp.cancel = context.WithCancelCause(m.Ctx)

	options := downloadOptions{
		bufSize: m.opt.BufferSize,
		maxTry:  m.opt.MaxRetry,
		timeout: m.getTimeout(),
		sleep:   time.Duration(m.opt.SpeedLimit*50) * time.Millisecond,
		patcher: patcher,
	}

	for i, p := range session.Parts {
		if err := p.init(i+1, session); err != nil {
			return err
		}
		// at ContentLength = 0 p.isDone() is always true therefore we shouldn't skip written = 0 part
		if p.Written != 0 && p.isDone() {
			atomic.AddUint32(&doneCount, 1)
			continue
		}
		p.ctx, p.cancel = context.WithCancel(m.Ctx)
		p.firstResp = firstResp
		p.progress = progress
		p.single = session.Single
		// p := p // NOTE: uncomment for Go < 1.22, see /doc/faq#closures_and_goroutines
		eg.Go(func() (err error) {
			defer func() {
				if v := recover(); v != nil {
					recoverHandler.Do(func() {
						for _, p := range session.Parts {
							if p.cancel != nil {
								p.cancel()
							}
						}
						recovered = true
					})
					if e, ok := v.(error); ok {
						err = fmt.Errorf("%s recovered: %w", p.name, e)
					} else {
						err = fmt.Errorf("%s recovered: %v", p.name, v)
					}
					return
				}
				if !p.single && p.isDone() {
					atomic.AddUint32(&doneCount, 1)
				}
			}()
			return p.download(m.Err, session.location, options)
		})
	}

	go func() {
		firstResp.cancel(eg.Wait())
	}()

	<-firstResp.ctx.Done()
	start, cause := time.Now(), context.Cause(firstResp.ctx)
	m.loggers[DBUG].Printf("First Response: %v", cause)

	switch {
	case errors.Is(cause, modeFallback):
		if session.restored {
			for _, p := range session.Parts {
				if p.cancel != nil {
					p.cancel()
				}
			}
			_ = eg.Wait()
			for _, p := range session.Parts {
				if p.file != nil {
					_ = p.file.Close()
				}
			}
			err := fmt.Errorf("restored session is expected to get status %d but got status %d instead", http.StatusPartialContent, http.StatusOK)
			panic(err)
		}
		err = eg.Wait()
		if !session.Single {
			id := <-firstResp.id
			session.Parts[0], session.Parts = session.Parts[id-1], session.Parts[:1]
			session.Single = true
		}
	case errors.Is(cause, modePartial) && !session.Single:
		progress.runTotalBar(session.ContentLength, &doneCount, len(session.Parts), start.Add(-session.Elapsed))
		fallthrough
	default:
		err = eg.Wait()
		session.Elapsed += time.Since(start)
	}

	if err != nil {
		for _, p := range session.Parts {
			if f := p.file; f != nil {
				m.loggers[DBUG].Printf("%q closed with: %v", f.Name(), f.Close())
				if session.Single && !session.isResumable() {
					err := os.Rename(f.Name(), session.OutputName)
					m.loggers[DBUG].Printf("%q renamed to %q with: %v", f.Name(), session.OutputName, err)
				}
			}
		}
		return withStack(cmp.Or(context.Cause(m.Ctx), err))
	}

	var output *os.File
	if session.Single {
		output = session.Parts[0].file
	} else {
		output, err = concatenate(session.Parts, progress, m.loggers[DBUG])
		if err != nil {
			return withStack(err)
		}
	}
	if session.isResumable() {
		if stat, err := output.Stat(); err == nil {
			if session.ContentLength != stat.Size() {
				return withStack(ContentMismatch[int64]{
					kind: "Length",
					old:  session.ContentLength,
					new:  stat.Size(),
				})
			}
		}
		if m.opt.SessionName != "" {
			m.loggers[DBUG].Printf("%q removed with: %v", m.opt.SessionName, os.Remove(m.opt.SessionName))
		}
	}
	if err := cmp.Or(output.Sync(), output.Close(), os.Rename(output.Name(), session.OutputName)); err != nil {
		return withStack(err)
	}
	m.loggers[DBUG].Printf("%q renamed to %q", output.Name(), session.OutputName)

	return nil
}

func (m *Cmd) init() {
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
}

func (m *Cmd) newRequestPatcher() (*requestPatcher, error) {
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

func (m *Cmd) getState(patcher *requestPatcher) (session *Session, err error) {
	var client *http.Client
	defer func() {
		if err == nil && client != nil {
			client.CheckRedirect = nil
			httpClient = client
		}
		err = withMessage(err, "getState")
	}()
	rtBuilder := newRoundTripperBuilder(m.loggers[DBUG])
	if m.opt.Proxy != "" {
		fixedURL, err := url.Parse(m.opt.Proxy)
		if err != nil {
			return nil, withStack(BadProxyURL{err})
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
	patcher.setHeaders(m.opt.HeaderMap)
	patcher.setUserAgent(userAgents[m.opt.UserAgent])
	for {
		switch {
		case m.opt.SessionName != "":
			restored := new(Session)
			if err := restored.loadState(m.opt.SessionName); err != nil {
				return nil, withStack(err)
			}
			m.loggers[DBUG].Printf("Session restored from: %q", m.opt.SessionName)
			m.opt.Output.Name = restored.OutputName
			patcher.setHeaders(restored.HeaderMap)
			if m.opt.UserAgent != "" {
				patcher.setUserAgent(userAgents[m.opt.UserAgent])
			}
			if client == nil {
				client = &http.Client{
					Jar:       jar,
					Transport: rtBuilder.pool(true).build(),
				}
			}
			if session != nil && restored.URL != session.URL {
				m.loggers[DBUG].Printf("Updating session.URL from: %q to: %q", restored.URL, session.URL)
				restored.URL = session.URL
			}
			// re-follow with patcher set to restored.HeaderMap
			session, err = m.follow(patcher, client, restored.URL)
			if err != nil {
				return nil, err
			}
			if restored.ContentLength != session.ContentLength {
				return nil, withStack(ContentMismatch[int64]{
					kind: "Length",
					old:  restored.ContentLength,
					new:  session.ContentLength,
				})
			}
			if restored.ContentType != session.ContentType {
				return nil, withStack(ContentMismatch[string]{
					kind: "Type",
					old:  restored.ContentType,
					new:  session.ContentType,
				})
			}
			session.Single = restored.Single
			session.Parts = restored.Parts
			session.Elapsed = restored.Elapsed
			session.HeaderMap = restored.HeaderMap
			session.restored = true
			return session, nil
		case m.opt.BestMirror.Mirrors != "":
			top, err := m.bestMirror(patcher, &http.Client{
				Transport: rtBuilder.pool(false).build(),
			})
			if err != nil {
				return nil, withStack(err)
			}
			for _, mirror := range top[:cmp.Or(min(m.opt.BestMirror.TopN, uint(len(top))), uint(len(top)))] {
				m.loggers[INFO].Println(mirror.avgDur.Truncate(time.Microsecond), mirror.url)
			}
			if m.opt.BestMirror.TopN != 1 {
				return nil, ErrCanceledByUser
			}
			m.opt.Positional.Location = top[0].url
			fallthrough
		case m.opt.Positional.Location != "":
			client = &http.Client{
				Jar:       jar,
				Transport: rtBuilder.pool(true).build(),
			}
			session, err = m.follow(patcher, client, m.opt.Positional.Location)
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
			if !session.isResumable() && m.opt.Parts != 0 {
				m.opt.Parts = 1
			}
			session.Parts, err = makeParts(m.opt.Parts, session.ContentLength)
			if err != nil {
				return session, withStack(err)
			}
			exist, err = isFileExist(session.OutputName)
			if err != nil {
				return nil, withStack(err)
			}
			session.Single = m.opt.Parts == 1
			session.HeaderMap = m.opt.HeaderMap
			if !exist {
				return session, nil
			}
			if m.opt.Output.Overwrite {
				err := os.Remove(session.OutputName)
				m.loggers[DBUG].Printf("%q removed with: %v", session.OutputName, err)
				return session, withStack(err)
			}
			return session, withStack(m.confirmFileOverwrite(session.OutputName))
		default:
			return nil, new(flags.Error)
		}
	}
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

func (m Cmd) follow(patcher httpRequestPatcher, client *http.Client, rawURL string) (session *Session, err error) {
	var redirected bool
	defer func() {
		if redirected {
			client.CloseIdleConnections()
		}
		client.CheckRedirect = nil
		err = withMessage(err, "follow")
	}()

	client.CheckRedirect = func(*http.Request, []*http.Request) error {
		return http.ErrUseLastResponse
	}

	location := rawURL
	timeout := m.getTimeout()
	template := "GET:R%02d %%s"
	maxRedirect := cmp.Or(m.opt.MaxRedirect, math.MaxUint)

	err = backoff.RetryWithContext(m.Ctx, exponential.New(exponential.WithBaseDelay(500*time.Millisecond)),
		func(attempt uint, _ func()) (bool, error) {
			ctx, cancel := context.WithTimeout(m.Ctx, timeout)
			defer func() {
				if errors.Is(ctx.Err(), context.DeadlineExceeded) {
					timeout += 5 * time.Second
					timeout = min(timeout, maxTimeout*time.Second)
				}
				cancel()
			}()
			template := fmt.Sprintf(template, attempt)
			for i := uint(0); i <= maxRedirect; i++ {
				m.loggers[INFO].Printf(template, location)
				m.loggers[DBUG].Printf(template, location)

				req, err := http.NewRequest(http.MethodGet, location, nil)
				if err != nil {
					return false, withStack(err)
				}

				patcher.patch(req)

				for k, v := range req.Header {
					m.loggers[DBUG].Printf("Request Header: %s: %v", k, v)
				}

				resp, err := client.Do(req.WithContext(ctx))
				if err != nil {
					m.loggers[WARN].Println(unwrapOrErr(err).Error())
					m.loggers[DBUG].Println(err.Error())
					if attempt != 0 && attempt == m.opt.MaxRetry {
						return false, ErrMaxRetry
					}
					return true, err
				}

				if jar := client.Jar; jar != nil {
					for _, cookie := range jar.Cookies(req.URL) {
						m.loggers[DBUG].Println("Cookie:", cookie) // cookie implements fmt.Stringer
					}
				}

				m.loggers[DBUG].Println("Response Status:", resp.Status)

				for k, v := range resp.Header {
					m.loggers[DBUG].Printf("Response Header: %s: %v", k, v)
				}

				if isRedirect(resp.StatusCode) {
					m.loggers[INFO].Println("Response Status:", resp.Status)
					redirected = true
					loc, err := resp.Location()
					if err != nil {
						return false, withStack(err)
					}
					location = loc.String()
					if resp.Body != nil {
						err := resp.Body.Close()
						if err != nil {
							return false, withStack(err)
						}
					}
					continue
				}

				if resp.StatusCode != http.StatusOK {
					err := withStack(UnexpectedHttpStatus(resp.StatusCode))
					m.loggers[WARN].Println(err.Error())
					if isServerError(resp.StatusCode) { // server error may be temporary
						return attempt != m.opt.MaxRetry, err
					}
					return false, err // err is already with stack
				}

				m.loggers[INFO].Println("Response Status:", resp.Status)

				for i := 2; m.opt.Output.Name == "" && i >= 0; i-- {
					if i == 0 {
						m.opt.Output.Name = "unknown"
						continue
					}
					if m.opt.Output.PathFirst {
						path := location
						nURL, err := url.Parse(location)
						switch {
						case err == nil && nURL.Path != "":
							path = nURL.Path
						case err == nil && nURL.Opaque != "":
							path = nURL.Opaque
							fallthrough
						default:
							unescaped, err := url.QueryUnescape(path)
							if err == nil {
								path = unescaped
							}
						}
						m.opt.Output.Name = filepath.Base(path)
						m.opt.Output.PathFirst = false
					} else {
						m.opt.Output.Name = parseContentDisposition(resp.Header.Get(hContentDisposition))
						m.opt.Output.PathFirst = true
					}
				}

				session = &Session{
					URL:           rawURL,
					OutputName:    m.opt.Output.Name,
					AcceptRanges:  resp.Header.Get(hAcceptRanges),
					ContentType:   resp.Header.Get(hContentType),
					StatusCode:    resp.StatusCode,
					ContentLength: resp.ContentLength,
					location:      location,
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

func (m Cmd) confirmFileOverwrite(name string) error {
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
		m.loggers[DBUG].Printf("%q removed with: %v", name, err)
		return err
	default:
		return ErrCanceledByUser
	}
}

func (m Cmd) getTimeout() time.Duration {
	timeout := min(cmp.Or(m.opt.Timeout, maxTimeout), maxTimeout)
	return time.Duration(timeout) * time.Second
}

func concatenate(parts []*Part, progress *progress, logger *log.Logger) (*os.File, error) {
	bar, err := progress.addConcatBar(len(parts))
	if err != nil {
		return nil, err
	}
	defer bar.Abort(false)

	files := make([]*catFile, 0, len(parts))
	for _, p := range parts {
		files = append(files, &catFile{p.output, p.file})
	}

	return files[0].file, cat(files, bar, logger)
}

// https://go.dev/play/p/Q25_gze66yB
func cat(files []*catFile, bar *mpb.Bar, logger *log.Logger) error {
	if len(files) < 2 {
		return nil
	}

	var eg errgroup.Group
	for i := 2; i <= len(files); i += 2 {
		dst, src := files[i-2], files[i-1]
		// dst.file can be nil when session is restored with some complete part
		if dst.file == nil {
			file, err := os.OpenFile(dst.name, os.O_WRONLY|os.O_APPEND, umask)
			if err != nil {
				return err
			}
			dst.file = file
			logger.Printf("%q dst reopen ok", dst.name)
		}
		// have to reopen src.file for reading
		if src.file != nil {
			if err := src.file.Close(); err != nil {
				return err
			}
		}
		file, err := os.Open(src.name)
		if err != nil {
			return err
		}
		src.file, files[i-1] = file, nil
		logger.Printf("%q src reopen ok", src.name)

		eg.Go(func() error {
			defer bar.Increment()

			n, err := io.Copy(dst.file, src.file)
			if err != nil {
				return err
			}
			logger.Printf("%d bytes copied: dst=%q src=%q", n, dst.name, src.name)

			err = cmp.Or(src.file.Close(), os.Remove(src.name))
			if err != nil {
				return err
			}
			logger.Printf("%q src remove ok", src.name)

			return nil
		})
	}

	if err := eg.Wait(); err != nil {
		for _, f := range files {
			if f != nil && f.file != nil {
				logger.Printf("%q closed with: %v", f.name, f.file.Close())
			}
		}
		return err
	}

	i := 0
	for _, f := range files {
		if f != nil {
			files[i] = f
			i++
		}
	}

	return cat(files[:i], bar, logger)
}

func parseContentDisposition(input string) (output string) {
	defer func() {
		if output != "" {
			unescaped, err := url.QueryUnescape(output)
			if err == nil {
				output = unescaped
			}
		}
	}()
	group := reContentDisposition.FindStringSubmatch(input)
	if len(group) != 3 {
		return
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
	return
}

func isRedirect(status int) bool {
	return status > 299 && status < 400
}

func isServerError(status int) bool {
	return status > 499 && status < 600
}

func makeStateQuery(session *Session, initialWritten int64) func(int64, error) sessionState {
	if !session.isResumable() {
		return func(_ int64, err error) sessionState {
			if err != nil {
				return sessionUncompleted
			}
			return sessionCompleted
		}
	}
	return func(written int64, err error) sessionState {
		if written != session.ContentLength {
			if written != initialWritten { // if some bytes were written
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

func makeParts(n uint, length int64) ([]*Part, error) {
	if n == 0 {
		return nil, ErrZeroParts
	}
	fragment := length / int64(n)
	if n != 1 && fragment < minFragment {
		return nil, ErrTooFragmented
	}

	parts := make([]*Part, 0, n)

	var offset int64
	for range n {
		p := &Part{
			Start: offset,
			Stop:  offset + fragment - 1,
		}
		offset += p.len()
		parts = append(parts, p)
	}

	if offset != length {
		last := parts[len(parts)-1]
		last.Stop = length - 1
	}

	return parts, nil
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

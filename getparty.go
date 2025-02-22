package getparty

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"errors"
	"fmt"
	"io"
	"log"
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

type (
	ExpectedError        string
	UnexpectedHttpStatus int
	singleModeFallback   int
	sessionState         int
)

const (
	sessionUncompleted sessionState = iota
	sessionUncompletedWithAdvance
	sessionCompletedWithError
	sessionCompleted
)

func (e ExpectedError) Error() string {
	return string(e)
}

func (e UnexpectedHttpStatus) Error() string {
	return fmt.Sprintf("Unexpected http status: %d", int(e))
}

func (e singleModeFallback) Error() string {
	return fmt.Sprintf("P%02d: fallback to single mode", int(e))
}

const (
	ErrBadInvariant   = ExpectedError("Bad invariant")
	ErrCanceledByUser = ExpectedError("Canceled by user")
	ErrMaxRedirect    = ExpectedError("Max redirections")
	ErrMaxRetry       = ExpectedError("Max retries")
	ErrZeroParts      = ExpectedError("No parts no work")
	ErrTooFragmented  = ExpectedError("Too many parts for such pathetic download")
	errUnexpectedOK   = UnexpectedHttpStatus(http.StatusOK)
)

const (
	cmdName     = "getparty"
	projectHome = "https://github.com/vbauerster/getparty"

	umask               = 0644
	maxTimeout          = 90
	refreshRate         = 200
	hUserAgentKey       = "User-Agent"
	hContentDisposition = "Content-Disposition"
	hContentMD5         = "Content-MD5"
	hAcceptRanges       = "Accept-Ranges"
	hContentType        = "Content-Type"
	hRange              = "Range"
	hCookie             = "Cookie"
	hHost               = "Host"
)

var (
	reContentDisposition = regexp.MustCompile(`filename[^;\n=]*=(['"](.*?)['"]|[^;\n]*)`) // https://regex101.com/r/N4AovD/3
	userAgents           = map[string]string{
		"chrome":  "Mozilla/5.0 (X11; Linux x86_64) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/128.0.0.0 Safari/537.36",
		"firefox": "Mozilla/5.0 (X11; Linux x86_64; rv:129.0) Gecko/20100101 Firefox/129.0",
		"safari":  "Mozilla/5.0 (Macintosh; Intel Mac OS X 11_4) AppleWebKit/605.1.15 (KHTML, like Gecko) Version/14.1 Safari/605.1.15",
		"edge":    "Mozilla/5.0 (Macintosh; Intel Mac OS X 11_4) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/91.0.4472.101 Safari/537.36 Edg/91.0.864.37",
	}
)

// options struct, represents cmd line options
type options struct {
	Parts       uint              `short:"p" long:"parts" value-name:"n" default:"1" description:"number of parts"`
	MaxRetry    uint              `short:"r" long:"max-retry" value-name:"n" default:"10" description:"max retries per each part, 0 for infinite"`
	MaxRedirect uint              `long:"max-redirect" value-name:"n" default:"10" description:"max redirections allowed, 0 for infinite"`
	Timeout     uint              `short:"t" long:"timeout" value-name:"sec" default:"15" description:"initial timeout to fill a buffer"`
	BufferSize  uint              `short:"b" long:"buf-size" value-name:"KiB" choice:"2" choice:"4" choice:"8" choice:"16" default:"8" description:"buffer size, prefer smaller for slow connection"`
	SpeedLimit  uint              `short:"l" long:"speed-limit" choice:"1" choice:"2" choice:"3" choice:"4" choice:"5" description:"speed limit gauge"`
	SessionName string            `short:"s" long:"session" value-name:"FILE" description:"session state of incomplete download, file with json extension"`
	UserAgent   string            `short:"U" long:"user-agent" choice:"chrome" choice:"firefox" choice:"safari" choice:"edge" description:"User-Agent header (default: getparty/ver)"`
	AuthUser    string            `long:"username" description:"basic http auth username"`
	AuthPass    string            `long:"password" description:"basic http auth password"`
	HeaderMap   map[string]string `short:"H" long:"header" value-name:"key:value" description:"http header, can be specified more than once"`
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
	} `group:"Best-mirror Options" namespace:"mirror"`
	Positional struct {
		Location string `positional-arg-name:"<url>" description:"http location"`
	} `positional-args:"yes"`
}

type Cmd struct {
	Ctx     context.Context
	Out     io.Writer
	Err     io.Writer
	opt     *options
	parser  *flags.Parser
	patcher func(*http.Request)
	loggers [lEVELS]*log.Logger
}

func (m Cmd) Exit(err error) (status int) {
	if err == nil {
		return 0
	}

	defer func() {
		switch status {
		case 0, 2, 4:
			return
		}
		if m.opt != nil && m.opt.Debug {
			m.loggers[DEBUG].Printf("ERROR: %s", err.Error())
			if e := (*stack)(nil); errors.As(err, &e) {
				_, err := m.Err.Write(e.stack)
				if err != nil {
					panic(err)
				}
			}
		}
	}()

	if e := ExpectedError(""); errors.As(err, &e) {
		if e == ErrBadInvariant {
			log.Default().Println(e.Error())
			return 4
		}
		m.loggers[ERRO].Println(e.Error())
		return 1
	}

	if e := (*flags.Error)(nil); errors.As(err, &e) {
		if e.Type == flags.ErrHelp {
			// cmd invoked with --help switch
			return 0
		}
		m.parser.WriteHelp(m.Err)
		return 2
	}

	m.loggers[ERRO].Println(err.Error())
	return 3
}

func (m *Cmd) Run(args []string, version, commit string) (err error) {
	defer func() {
		err = withMessage(err, "run")
	}()

	err = m.invariantCheck()
	if err != nil {
		return err
	}

	m.opt = new(options)
	m.parser = flags.NewParser(m.opt, flags.Default)
	_, err = m.parser.ParseArgs(args)
	if err != nil {
		return err
	}

	userAgents[""] = fmt.Sprintf("%s/%s", cmdName, version)

	if m.opt.Version {
		fmt.Fprintf(m.Out, "%s (%.7s) (%s)\n", userAgents[""], commit, runtime.Version())
		fmt.Fprintf(m.Out, "Project home: %s\n", projectHome)
		return nil
	}

	m.initLoggers()

	var userinfo *url.Userinfo
	if m.opt.AuthUser != "" {
		if m.opt.AuthPass == "" {
			fmt.Fprint(m.Out, "Enter password: ")
			pass, err := term.ReadPassword(int(os.Stdin.Fd()))
			if err != nil {
				return withStack(err)
			}
			if err := context.Cause(m.Ctx); err != nil {
				return err
			}
			m.opt.AuthPass = string(pass)
			fmt.Fprintln(m.Out)
		}
		userinfo = url.UserPassword(m.opt.AuthUser, m.opt.AuthPass)
		m.opt.AuthUser = ""
		m.opt.AuthPass = ""
	}

	tlsConfig, err := m.getTLSConfig()
	if err != nil {
		return err
	}

	m.opt.HeaderMap[hUserAgentKey] = userAgents[m.opt.UserAgent]
	m.patcher = makeReqPatcher(userinfo, m.opt.HeaderMap)
	rtBuilder := newRoundTripperBuilder(tlsConfig)

	if m.opt.BestMirror.Mirrors != "" {
		top, err := m.bestMirror(rtBuilder.pool(false).build())
		if err != nil {
			return err
		}
		if len(top) == 1 {
			m.opt.Positional.Location = top[0]
		} else {
			return nil
		}
	}

	if m.opt.Positional.Location == "" && m.opt.SessionName == "" {
		return new(flags.Error)
	}

	// All users of cookiejar should import "golang.org/x/net/publicsuffix"
	jar, err := cookiejar.New(&cookiejar.Options{PublicSuffixList: publicsuffix.List})
	if err != nil {
		return withStack(err)
	}
	client := &http.Client{
		Transport: rtBuilder.pool(m.opt.Parts > 1).build(),
		Jar:       jar,
	}
	session, err := m.getState(client)
	if err != nil {
		if err == ErrZeroParts && session != nil {
			session.summary(m.loggers)
		}
		return err
	}
	session.summary(m.loggers)

	m.loggers[INFO].Printf("Saving to: %q", session.OutputName)

	if session.restored {
		if m.opt.UserAgent != "" {
			session.HeaderMap[hUserAgentKey] = userAgents[m.opt.UserAgent]
		}
		m.patcher = makeReqPatcher(userinfo, session.HeaderMap)
	}

	var doneCount uint32
	var eg errgroup.Group
	var recoverHandler sync.Once
	var recovered bool
	timeout := m.getTimeout()
	sleep := time.Duration(m.opt.SpeedLimit*60) * time.Millisecond

	status := new(httpStatusContext)
	status.ctx, status.cancel = context.WithCancelCause(context.Background())
	status.ok, status.quit = make(chan int), make(chan struct{})

	progress := newProgress(m.Ctx, session, m.getOut(), m.getErr())
	stateQuery := makeStateQuery(session, progress.current)
	start := make(chan time.Time, 1)
	defer func() {
		select {
		case start := <-start:
			session.Elapsed += time.Since(start)
		default:
		}
		var name string
		if recovered {
			name = session.OutputName + ".panic"
		} else {
			name = session.OutputName + ".json"
		}
		switch tw := session.totalWritten(); stateQuery(tw, err) {
		case sessionUncompleted:
			progress.Wait()
		case sessionUncompletedWithAdvance:
			err := session.dumpState(name)
			progress.Wait()
			if err != nil {
				m.loggers[ERRO].Println("Session state save failure:", err.Error())
			} else {
				m.loggers[INFO].Printf("Session state saved to %q", name)
			}
		case sessionCompletedWithError:
			progress.Wait()
			m.loggers[ERRO].Println("Session completed with error:", err.Error())
		case sessionCompleted:
			progress.Wait()
			m.loggers[INFO].Printf("%q saved [%d/%d]", session.OutputName, session.ContentLength, tw)
		}
	}()

	for i, p := range session.Parts {
		err := p.init(i+1, session)
		if err != nil {
			return err
		}
		// at ContentLength = 0 p.isDone() is always true therefore we shouldn't skip written = 0 part
		if p.Written != 0 && p.isDone() {
			atomic.AddUint32(&doneCount, 1)
			continue
		}
		p.ctx, p.cancel = context.WithCancel(m.Ctx)
		p.client = client
		p.status = status
		p.progress = progress
		p.patcher = m.patcher
		p := p // https://golang.org/doc/faq#closures_and_goroutines
		eg.Go(func() (err error) {
			defer func() {
				if v := recover(); v != nil {
					err = nil
					recoverHandler.Do(func() {
						for _, p := range session.Parts {
							if p.cancel != nil {
								p.cancel()
							}
						}
						err = fmt.Errorf("%s panic: %#v", p.name, v)
						recovered = true
					})
				} else if !p.single && p.isDone() {
					atomic.AddUint32(&doneCount, 1)
				}
			}()
			return p.download(session.location, m.opt.BufferSize, m.opt.MaxRetry, sleep, timeout)
		})
	}

	go func() {
		select {
		case id := <-status.ok: // on http.StatusOK
			start <- time.Now()
			for _, p := range session.Parts {
				if p.id != id && p.cancel != nil {
					p.cancel()
				}
			}
			err := singleModeFallback(id)
			status.cancel(err)
			if session.restored && !session.Single {
				if p := session.Parts[id-1]; p.cancel != nil {
					p.cancel()
				}
				panic(fmt.Errorf("P%02d: got http status ok while restored session was partial", id))
			}
			if context.Cause(status.ctx) != err {
				if p := session.Parts[id-1]; p.cancel != nil {
					p.cancel()
				}
				panic(fmt.Errorf("%s failure: some other part got partial content first", err.Error()))
			}
		case <-status.ctx.Done():
			now := time.Now()
			start <- now
			if !session.Single {
				progress.runTotalBar(session.ContentLength, &doneCount, len(session.Parts), now.Add(-session.Elapsed))
			}
		case <-status.quit:
		}
	}()

	err = eg.Wait()
	close(status.quit)
	status.cancel(nil)

	if id, ok := context.Cause(status.ctx).(singleModeFallback); ok && !session.Single {
		session.Parts[0], session.Parts = session.Parts[int(id)-1], session.Parts[:1]
		session.Single = true
	}
	if err != nil {
		for _, p := range session.Parts {
			if f := p.file; f != nil {
				m.loggers[DEBUG].Printf("%q closed with: %v", f.Name(), firstErr(f.Sync(), f.Close()))
			}
		}
		return firstErr(context.Cause(m.Ctx), err)
	}
	if !session.Single {
		err = m.concatenate(session.Parts, progress)
		if err != nil {
			return err
		}
	}
	if p := session.Parts[0]; p.file != nil {
		err = p.file.Sync()
		if err != nil {
			return withStack(err)
		}
		if session.isResumable() {
			stat, err := p.file.Stat()
			if err != nil {
				return withStack(err)
			}
			if size := stat.Size(); size != session.ContentLength {
				return withStack(fmt.Errorf("ContentLength mismatch: expected %d got %d", session.ContentLength, size))
			}
			if m.opt.SessionName != "" {
				err := os.Remove(m.opt.SessionName)
				if err != nil {
					return withStack(err)
				}
				m.loggers[DEBUG].Printf("%q remove ok", m.opt.SessionName)
			}
		}
		err = p.file.Close()
		if err != nil {
			return withStack(err)
		}
		m.loggers[DEBUG].Printf("%q close ok", p.file.Name())
		err = os.Rename(p.file.Name(), session.OutputName)
		if err != nil {
			return withStack(err)
		}
		m.loggers[DEBUG].Printf("%q rename to %q ok", p.file.Name(), session.OutputName)
	}
	return nil
}

func (m Cmd) getTLSConfig() (*tls.Config, error) {
	if m.opt.Https.CertsFileName != "" {
		buf, err := os.ReadFile(m.opt.Https.CertsFileName)
		if err != nil {
			return nil, withStack(err)
		}
		pool, err := x509.SystemCertPool()
		if err != nil {
			return nil, withStack(err)
		}
		if ok := pool.AppendCertsFromPEM(buf); !ok {
			return nil, withStack(fmt.Errorf("bad cert file %q", m.opt.Https.CertsFileName))
		}
		return &tls.Config{
			InsecureSkipVerify: m.opt.Https.InsecureSkipVerify,
			RootCAs:            pool,
		}, nil
	}
	if m.opt.Https.InsecureSkipVerify {
		return &tls.Config{InsecureSkipVerify: true}, nil
	}
	return nil, nil
}

func (m Cmd) getState(client *http.Client) (session *Session, err error) {
	client.CheckRedirect = func(_ *http.Request, via []*http.Request) error {
		max := int(m.opt.MaxRedirect)
		if max != 0 && len(via) > max {
			return ErrMaxRedirect
		}
		return http.ErrUseLastResponse
	}
	defer func() {
		client.CheckRedirect = nil
	}()
	for {
		switch {
		case m.opt.SessionName != "":
			restored := new(Session)
			err = restored.loadState(m.opt.SessionName)
			if err != nil {
				return nil, err
			}
			switch {
			case session == nil && restored.Redirected:
				session, err = m.follow(client, restored.URL)
				if err != nil {
					return nil, err
				}
				fallthrough
			case session != nil:
				if restored.ContentLength != session.ContentLength {
					return nil, fmt.Errorf("ContentLength mismatch: expected %d got %d", restored.ContentLength, session.ContentLength)
				}
				restored.location = session.location
			default:
				restored.location = restored.URL
			}
			restored.restored = true
			m.loggers[DEBUG].Printf("Session restored from: %q", m.opt.SessionName)
			return restored, nil
		case m.opt.Positional.Location != "":
			session, err = m.follow(client, m.opt.Positional.Location)
			if err != nil {
				return nil, err
			}
			session.HeaderMap = m.opt.HeaderMap
			state := session.OutputName + ".json"
			if _, err := os.Stat(state); errors.Is(err, os.ErrNotExist) {
				err := session.calcParts(m.opt.Parts)
				if err != nil {
					return session, err
				}
				exist, err := session.isOutputFileExist()
				if err != nil {
					return nil, err
				}
				if exist {
					return session, m.overwriteIfConfirmed(session.OutputName)
				}
				return session, nil
			}
			m.loggers[DEBUG].Printf("Reusing existing state: %q", state)
			m.opt.SessionName = state
		}
	}
}

func (m Cmd) follow(client *http.Client, rawURL string) (session *Session, err error) {
	var redirected bool
	defer func() {
		if redirected {
			client.CloseIdleConnections()
		}
		err = withMessage(err, "follow")
	}()

	if client.CheckRedirect == nil {
		return nil, withStack(errors.New("expected non nil client.CheckRedirect"))
	}

	location := rawURL
	timeout := m.getTimeout()
	template := "GET:R%02d %%q"

	err = backoff.RetryWithContext(m.Ctx, exponential.New(exponential.WithBaseDelay(500*time.Millisecond)),
		func(attempt uint, _ func()) (bool, error) {
			ctx, cancel := context.WithTimeout(m.Ctx, timeout)
			defer func() {
				cancel()
				if timeout < maxTimeout*time.Second {
					timeout += 5 * time.Second
				}
			}()
			getR := fmt.Sprintf(template, attempt)
			for {
				m.loggers[INFO].Printf(getR, location)
				m.loggers[DEBUG].Printf(getR, location)

				req, err := http.NewRequest(http.MethodGet, location, nil)
				if err != nil {
					return false, withStack(err)
				}

				if m.patcher != nil {
					m.patcher(req)
				}

				for k, v := range req.Header {
					m.loggers[DEBUG].Printf("Request Header: %s: %v", k, v)
				}

				resp, err := client.Do(req.WithContext(ctx))
				if err != nil {
					m.loggers[WARN].Println(unwrapOrErr(err).Error())
					m.loggers[DEBUG].Println(err.Error())
					if attempt != 0 && attempt == m.opt.MaxRetry {
						return false, ErrMaxRetry
					}
					if err == ErrMaxRedirect {
						return false, err
					}
					return true, err
				}

				if jar := client.Jar; jar != nil {
					for _, cookie := range jar.Cookies(req.URL) {
						m.loggers[DEBUG].Println("Cookie:", cookie) // cookie implements fmt.Stringer
					}
				}

				m.loggers[DEBUG].Println("HTTP response:", resp.Status)

				for k, v := range resp.Header {
					m.loggers[DEBUG].Printf("Response Header: %s: %v", k, v)
				}

				if isRedirect(resp.StatusCode) {
					m.loggers[INFO].Println("HTTP response:", resp.Status)
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

				m.loggers[INFO].Println("HTTP response:", resp.Status)

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
					location:      location,
					URL:           rawURL,
					OutputName:    m.opt.Output.Name,
					ContentMD5:    resp.Header.Get(hContentMD5),
					AcceptRanges:  resp.Header.Get(hAcceptRanges),
					ContentType:   resp.Header.Get(hContentType),
					StatusCode:    resp.StatusCode,
					ContentLength: resp.ContentLength,
					Redirected:    redirected,
				}

				return false, withStack(resp.Body.Close())
			}
		})
	return session, err
}

func (m Cmd) overwriteIfConfirmed(name string) (err error) {
	if m.opt.Output.Overwrite {
		m.loggers[DEBUG].Printf("Removing existing: %q", name)
		return withStack(os.Remove(name))
	}
	fmt.Fprintf(m.Err, "%q already exists, overwrite? [Y/n] ", name)
	state, err := term.MakeRaw(int(os.Stdin.Fd()))
	if err != nil {
		return withStack(err)
	}
	defer func() {
		err = firstErr(err, withStack(term.Restore(int(os.Stdin.Fd()), state)))
		fmt.Fprintln(m.Err)
	}()
	b := make([]byte, 1)
	_, err = os.Stdin.Read(b)
	if err != nil {
		return withStack(err)
	}
	switch b[0] {
	case 'y', 'Y', '\r':
		m.loggers[DEBUG].Printf("Removing existing: %q", name)
		return withStack(os.Remove(name))
	default:
		return ErrCanceledByUser
	}
}

func (m Cmd) getTimeout() time.Duration {
	if m.opt.Timeout == 0 {
		return 15 * time.Second
	}
	if m.opt.Timeout > maxTimeout {
		return maxTimeout * time.Second
	}
	return time.Duration(m.opt.Timeout) * time.Second
}

func (m Cmd) getOut() io.Writer {
	if m.opt == nil || m.opt.Quiet {
		return io.Discard
	}
	return m.Out
}

func (m Cmd) getErr() io.Writer {
	if m.opt != nil && m.opt.Debug {
		return m.Err
	}
	return io.Discard
}

func (m Cmd) invariantCheck() error {
	if m.Ctx == nil || m.Out == nil || m.Err == nil {
		return ErrBadInvariant
	}
	return nil
}

func (m Cmd) concatenate(parts []*Part, progress *progress) error {
	bar, err := progress.addConcatBar(len(parts))
	if err != nil {
		return withStack(err)
	}

	files := make([]*os.File, 0, len(parts))
	for _, p := range parts {
		// p.file is nil when session is restored with some complete part
		if p.file == nil {
			p.file, err = os.OpenFile(p.output, os.O_WRONLY|os.O_APPEND, umask)
			if err != nil {
				return withStack(err)
			}
			m.loggers[DEBUG].Printf("%q reopen nil file ok", p.file.Name())
		}
		files = append(files, p.file)
	}

	err = m.concat(files, bar)
	if err != nil {
		bar.Abort(false)
		return withStack(err)
	}

	return nil
}

// https://go.dev/play/p/ibK4VcNYa72
func (m Cmd) concat(files []*os.File, bar *mpb.Bar) error {
	if len(files) == 1 {
		return nil
	}

	var eg errgroup.Group
	for i := 2; i <= len(files); i += 2 {
		pair := files[i-2 : i]
		eg.Go(func() error {
			defer bar.Increment()
			return concat(pair, m.loggers[DEBUG])
		})
	}

	err := eg.Wait()
	if err != nil {
		return err
	}

	var x []*os.File
	for _, file := range files {
		if file != nil {
			x = append(x, file)
		}
	}

	return m.concat(x, bar)
}

func concat(pair []*os.File, logger *log.Logger) error {
	if len(pair) != 2 {
		return fmt.Errorf("unexpected pair len: %d", len(pair))
	}
	// The behavior of Seek on a file opened with O_APPEND is not specified.
	// Have to reopen file which was initially opened with O_APPEND flag.
	dst, src := pair[0], pair[1]
	err := src.Close()
	if err != nil {
		return err
	}
	src, err = os.Open(src.Name())
	if err != nil {
		return err
	}
	logger.Printf("%q reopen ok", src.Name())

	n, err := io.Copy(dst, src)
	if err != nil {
		return err
	}
	logger.Printf("%d bytes copied: dst=%q src=%q", n, dst.Name(), src.Name())

	err = src.Close()
	if err != nil {
		return err
	}
	logger.Printf("%q close ok", src.Name())

	err = os.Remove(src.Name())
	if err != nil {
		return err
	}
	logger.Printf("%q remove ok", src.Name())

	pair[1] = nil
	return nil
}

func makeReqPatcher(userinfo *url.Userinfo, headers map[string]string) func(*http.Request) {
	return func(req *http.Request) {
		if req == nil {
			return
		}
		if req.URL != nil {
			req.URL.User = userinfo
		}
		for k, v := range headers {
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
	if found && strings.ToLower(b) == "utf-8" {
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

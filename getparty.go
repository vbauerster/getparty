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

func (cmd Cmd) Exit(err error) (status int) {
	if err == nil {
		return 0
	}

	defer func() {
		switch status {
		case 0, 2, 4:
			return
		}
		if cmd.opt != nil && cmd.opt.Debug {
			cmd.loggers[DEBUG].Printf("ERROR: %s", err.Error())
			if e := (*stack)(nil); errors.As(err, &e) {
				_, err := cmd.Err.Write(e.stack)
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
		cmd.loggers[ERRO].Println(e.Error())
		return 1
	}

	if e := (*flags.Error)(nil); errors.As(err, &e) {
		if e.Type == flags.ErrHelp {
			// cmd invoked with --help switch
			return 0
		}
		cmd.parser.WriteHelp(cmd.Err)
		return 2
	}

	cmd.loggers[ERRO].Println(err.Error())
	return 3
}

func (cmd *Cmd) Run(args []string, version, commit string) (err error) {
	defer func() {
		err = withMessage(err, "run")
	}()

	err = cmd.invariantCheck()
	if err != nil {
		return err
	}

	cmd.opt = new(options)
	cmd.parser = flags.NewParser(cmd.opt, flags.Default)
	_, err = cmd.parser.ParseArgs(args)
	if err != nil {
		return err
	}

	userAgents[""] = fmt.Sprintf("%s/%s", cmdName, version)

	if cmd.opt.Version {
		fmt.Fprintf(cmd.Out, "%s (%.7s) (%s)\n", userAgents[""], commit, runtime.Version())
		fmt.Fprintf(cmd.Out, "Project home: %s\n", projectHome)
		return nil
	}

	cmd.initLoggers()

	var userinfo *url.Userinfo
	if cmd.opt.AuthUser != "" {
		if cmd.opt.AuthPass == "" {
			fmt.Fprint(cmd.Out, "Enter password: ")
			pass, err := term.ReadPassword(int(os.Stdin.Fd()))
			if err != nil {
				return withStack(err)
			}
			if err := context.Cause(cmd.Ctx); err != nil {
				return err
			}
			cmd.opt.AuthPass = string(pass)
			fmt.Fprintln(cmd.Out)
		}
		userinfo = url.UserPassword(cmd.opt.AuthUser, cmd.opt.AuthPass)
		cmd.opt.AuthUser = ""
		cmd.opt.AuthPass = ""
	}

	tlsConfig, err := cmd.getTLSConfig()
	if err != nil {
		return err
	}

	cmd.opt.HeaderMap[hUserAgentKey] = userAgents[cmd.opt.UserAgent]
	cmd.patcher = makeReqPatcher(userinfo, cmd.opt.HeaderMap)
	rtBuilder := newRoundTripperBuilder(tlsConfig)

	if cmd.opt.BestMirror.Mirrors != "" {
		top, err := cmd.bestMirror(rtBuilder.pool(false).build())
		if err != nil {
			return err
		}
		if len(top) == 1 {
			cmd.opt.Positional.Location = top[0]
		} else {
			return nil
		}
	}

	if cmd.opt.Positional.Location == "" && cmd.opt.SessionName == "" {
		return new(flags.Error)
	}

	// All users of cookiejar should import "golang.org/x/net/publicsuffix"
	jar, err := cookiejar.New(&cookiejar.Options{PublicSuffixList: publicsuffix.List})
	if err != nil {
		return withStack(err)
	}
	client := &http.Client{
		Transport: rtBuilder.pool(cmd.opt.Parts > 1).build(),
		Jar:       jar,
	}
	session, err := cmd.getState(client)
	if err != nil {
		if err == ErrZeroParts && session != nil {
			session.summary(cmd.loggers)
		}
		return err
	}
	session.summary(cmd.loggers)

	cmd.loggers[INFO].Printf("Saving to: %q", session.OutputName)

	if session.restored {
		if cmd.opt.UserAgent != "" {
			session.HeaderMap[hUserAgentKey] = userAgents[cmd.opt.UserAgent]
		}
		cmd.patcher = makeReqPatcher(userinfo, session.HeaderMap)
	}

	var doneCount uint32
	var eg errgroup.Group
	var recoverHandler sync.Once
	var recovered bool
	timeout := cmd.getTimeout()
	sleep := time.Duration(cmd.opt.SpeedLimit*60) * time.Millisecond

	status := new(httpStatusContext)
	status.ctx, status.cancel = context.WithCancelCause(context.Background())
	status.ok, status.quit = make(chan int), make(chan struct{})

	debugOut := cmd.getErr()
	progress := session.newProgress(cmd.Ctx, cmd.getOut(), debugOut)
	stateQuery := cmd.makeStateQuery(session, progress.written)
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
				cmd.loggers[ERRO].Println("Session state save failure:", err.Error())
			} else {
				cmd.loggers[INFO].Printf("Session state saved to %q", name)
			}
		case sessionCompletedWithError:
			progress.Wait()
			cmd.loggers[ERRO].Println("Session completed with error:", err.Error())
		case sessionCompleted:
			progress.Wait()
			cmd.loggers[INFO].Printf("%q saved [%d/%d]", session.OutputName, session.ContentLength, tw)
		}
	}()

	for i, p := range session.Parts {
		err := p.init(i+1, session, debugOut)
		if err != nil {
			return err
		}
		// at ContentLength = 0 p.isDone() is always true therefore we shouldn't skip written = 0 part
		if p.Written != 0 && p.isDone() {
			atomic.AddUint32(&doneCount, 1)
			continue
		}
		p.ctx, p.cancel = context.WithCancel(cmd.Ctx)
		p.client = client
		p.status = status
		p.progress = progress
		p.patcher = cmd.patcher
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
					return
				}
				if !p.single && p.isDone() {
					atomic.AddUint32(&doneCount, 1)
				}
			}()
			return p.download(session.location, cmd.opt.BufferSize, cmd.opt.MaxRetry, sleep, timeout)
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
				session.runTotalBar(progress, &doneCount, now)
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
		return firstErr(context.Cause(cmd.Ctx), err)
	}
	if !session.Single {
		err = session.concatenateParts(progress)
		if err != nil {
			return err
		}
	}
	if cmd.opt.SessionName != "" {
		return withStack(os.Remove(cmd.opt.SessionName))
	}
	return nil
}

func (cmd Cmd) makeStateQuery(session *Session, initialWritten int64) func(int64, error) sessionState {
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

func (cmd Cmd) getTLSConfig() (*tls.Config, error) {
	if cmd.opt.Https.CertsFileName != "" {
		buf, err := os.ReadFile(cmd.opt.Https.CertsFileName)
		if err != nil {
			return nil, withStack(err)
		}
		pool, err := x509.SystemCertPool()
		if err != nil {
			return nil, withStack(err)
		}
		if ok := pool.AppendCertsFromPEM(buf); !ok {
			return nil, withStack(fmt.Errorf("bad cert file %q", cmd.opt.Https.CertsFileName))
		}
		return &tls.Config{
			InsecureSkipVerify: cmd.opt.Https.InsecureSkipVerify,
			RootCAs:            pool,
		}, nil
	}
	if cmd.opt.Https.InsecureSkipVerify {
		return &tls.Config{InsecureSkipVerify: true}, nil
	}
	return nil, nil
}

func (cmd Cmd) getState(client *http.Client) (session *Session, err error) {
	client.CheckRedirect = func(_ *http.Request, via []*http.Request) error {
		max := int(cmd.opt.MaxRedirect)
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
		case cmd.opt.SessionName != "":
			restored := new(Session)
			err = restored.loadState(cmd.opt.SessionName)
			if err != nil {
				return nil, err
			}
			switch {
			case session == nil && restored.Redirected:
				session, err = cmd.follow(client, restored.URL)
				if err != nil {
					return nil, err
				}
				fallthrough
			case session != nil:
				err = restored.checkContentSums(*session)
				if err != nil {
					return nil, err
				}
				restored.location = session.location
			default:
				restored.location = restored.URL
			}
			restored.restored = true
			cmd.loggers[DEBUG].Printf("Session restored from: %q", cmd.opt.SessionName)
			return restored, nil
		case cmd.opt.Positional.Location != "":
			session, err = cmd.follow(client, cmd.opt.Positional.Location)
			if err != nil {
				return nil, err
			}
			session.HeaderMap = cmd.opt.HeaderMap
			state := session.OutputName + ".json"
			if _, err := os.Stat(state); errors.Is(err, os.ErrNotExist) {
				err := session.calcParts(cmd.opt.Parts)
				if err != nil {
					return session, err
				}
				exist, err := session.isOutputFileExist()
				if err != nil {
					return nil, err
				}
				if exist {
					return session, cmd.overwriteIfConfirmed(session.OutputName)
				}
				return session, nil
			}
			cmd.loggers[DEBUG].Printf("Reusing existing state: %q", state)
			cmd.opt.SessionName = state
		}
	}
}

func (cmd Cmd) follow(client *http.Client, rawURL string) (session *Session, err error) {
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
	timeout := cmd.getTimeout()
	template := "GET:R%02d %%q"

	err = backoff.RetryWithContext(cmd.Ctx, exponential.New(exponential.WithBaseDelay(500*time.Millisecond)),
		func(attempt uint, _ func()) (bool, error) {
			ctx, cancel := context.WithTimeout(cmd.Ctx, timeout)
			defer func() {
				cancel()
				if timeout < maxTimeout*time.Second {
					timeout += 5 * time.Second
				}
			}()
			getR := fmt.Sprintf(template, attempt)
			for {
				cmd.loggers[INFO].Printf(getR, location)
				cmd.loggers[DEBUG].Printf(getR, location)

				req, err := http.NewRequest(http.MethodGet, location, nil)
				if err != nil {
					return false, withStack(err)
				}

				if cmd.patcher != nil {
					cmd.patcher(req)
				}

				for k, v := range req.Header {
					cmd.loggers[DEBUG].Printf("Request Header: %s: %v", k, v)
				}

				resp, err := client.Do(req.WithContext(ctx))
				if err != nil {
					cmd.loggers[WARN].Println(unwrapOrErr(err).Error())
					cmd.loggers[DEBUG].Println(err.Error())
					if attempt != 0 && attempt == cmd.opt.MaxRetry {
						return false, ErrMaxRetry
					}
					if err == ErrMaxRedirect {
						return false, err
					}
					return true, err
				}

				if jar := client.Jar; jar != nil {
					for _, cookie := range jar.Cookies(req.URL) {
						cmd.loggers[DEBUG].Println("Cookie:", cookie) // cookie implements fmt.Stringer
					}
				}

				cmd.loggers[DEBUG].Println("HTTP response:", resp.Status)

				for k, v := range resp.Header {
					cmd.loggers[DEBUG].Printf("Response Header: %s: %v", k, v)
				}

				if isRedirect(resp.StatusCode) {
					cmd.loggers[INFO].Println("HTTP response:", resp.Status)
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
					cmd.loggers[WARN].Println(err.Error())
					if isServerError(resp.StatusCode) { // server error may be temporary
						return attempt != cmd.opt.MaxRetry, err
					}
					return false, err // err is already with stack
				}

				cmd.loggers[INFO].Println("HTTP response:", resp.Status)

				for i := 2; cmd.opt.Output.Name == "" && i >= 0; i-- {
					if i == 0 {
						cmd.opt.Output.Name = "unknown"
						continue
					}
					if cmd.opt.Output.PathFirst {
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
						cmd.opt.Output.Name = filepath.Base(path)
						cmd.opt.Output.PathFirst = false
					} else {
						cmd.opt.Output.Name = parseContentDisposition(resp.Header.Get(hContentDisposition))
						cmd.opt.Output.PathFirst = true
					}
				}

				session = &Session{
					location:      location,
					URL:           rawURL,
					OutputName:    cmd.opt.Output.Name,
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

func (cmd Cmd) overwriteIfConfirmed(name string) (err error) {
	if cmd.opt.Output.Overwrite {
		cmd.loggers[DEBUG].Printf("Removing existing: %q", name)
		return withStack(os.Remove(name))
	}
	fmt.Fprintf(cmd.Err, "%q already exists, overwrite? [Y/n] ", name)
	state, err := term.MakeRaw(int(os.Stdin.Fd()))
	if err != nil {
		return withStack(err)
	}
	defer func() {
		err = firstErr(err, withStack(term.Restore(int(os.Stdin.Fd()), state)))
		fmt.Fprintln(cmd.Err)
	}()
	b := make([]byte, 1)
	_, err = os.Stdin.Read(b)
	if err != nil {
		return withStack(err)
	}
	switch b[0] {
	case 'y', 'Y', '\r':
		cmd.loggers[DEBUG].Printf("Removing existing: %q", name)
		return withStack(os.Remove(name))
	default:
		return ErrCanceledByUser
	}
}

func (cmd Cmd) getTimeout() time.Duration {
	if cmd.opt.Timeout == 0 {
		return 15 * time.Second
	}
	if cmd.opt.Timeout > maxTimeout {
		return maxTimeout * time.Second
	}
	return time.Duration(cmd.opt.Timeout) * time.Second
}

func (cmd Cmd) getOut() io.Writer {
	if cmd.opt == nil || cmd.opt.Quiet {
		return io.Discard
	}
	return cmd.Out
}

func (cmd Cmd) getErr() io.Writer {
	if cmd.opt != nil && cmd.opt.Debug {
		return cmd.Err
	}
	return io.Discard
}

func (cmd Cmd) invariantCheck() error {
	if cmd.Ctx == nil || cmd.Out == nil || cmd.Err == nil {
		return ErrBadInvariant
	}
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

package getparty

import (
	"context"
	"crypto/tls"
	"crypto/x509"
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
	"github.com/pkg/errors"
	"github.com/vbauerster/backoff"
	"github.com/vbauerster/backoff/exponential"
	"github.com/vbauerster/mpb/v8"
	"golang.org/x/net/publicsuffix"
	"golang.org/x/sync/errgroup"
	"golang.org/x/term"
)

type ExpectedError string

func (e ExpectedError) Error() string {
	return string(e)
}

type BadHttpStatus int

func (e BadHttpStatus) Error() string {
	return fmt.Sprintf("Bad status: %d", int(e))
}

const (
	ErrBadInvariant   = ExpectedError("Bad invariant")
	ErrCanceledByUser = ExpectedError("Canceled by user")
	ErrMaxRedirect    = ExpectedError("Max redirections")
	ErrMaxRetry       = ExpectedError("Max retries")
)

const (
	cmdName     = "getparty"
	projectHome = "https://github.com/vbauerster/getparty"

	umask               = 0644
	maxTimeout          = 180
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
		"chrome":  "Mozilla/5.0 (X11; Linux x86_64) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/113.0.0.0 Safari/537.36",
		"firefox": "Mozilla/5.0 (X11; Linux x86_64; rv:109.0) Gecko/20100101 Firefox/112.0",
		"safari":  "Mozilla/5.0 (Macintosh; Intel Mac OS X 11_4) AppleWebKit/605.1.15 (KHTML, like Gecko) Version/14.1 Safari/605.1.15",
		"edge":    "Mozilla/5.0 (Macintosh; Intel Mac OS X 11_4) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/91.0.4472.101 Safari/537.36 Edg/91.0.864.37",
	}
)

// options struct, represents cmd line options
type options struct {
	Parts          uint              `short:"p" long:"parts" value-name:"n" default:"1" description:"number of parts"`
	MaxRetry       uint              `short:"r" long:"max-retry" value-name:"n" default:"10" description:"max retries per each part, 0 for infinite"`
	MaxRedirect    uint              `long:"max-redirect" value-name:"n" default:"10" description:"max redirections allowed, 0 for infinite"`
	Timeout        uint              `short:"t" long:"timeout" value-name:"sec" default:"15" description:"context timeout"`
	SpeedLimit     uint              `short:"l" long:"speed-limit" choice:"1" choice:"2" choice:"3" choice:"4" choice:"5" description:"speed limit gauge"`
	OutputName     string            `short:"o" long:"output" value-name:"FILE" description:"output file name"`
	SessionName    string            `short:"s" long:"session" value-name:"FILE" description:"session state of incomplete download, file with json extension"`
	UserAgent      string            `short:"U" long:"user-agent" choice:"chrome" choice:"firefox" choice:"safari" choice:"edge" description:"User-Agent header (default: getparty/ver)"`
	AuthUser       string            `long:"username" description:"basic http auth username"`
	AuthPass       string            `long:"password" description:"basic http auth password"`
	HeaderMap      map[string]string `short:"H" long:"header" value-name:"key:value" description:"http header, can be specified more than once"`
	ForceOverwrite bool              `short:"f" long:"force" description:"overwrite existing file silently"`
	Quiet          bool              `short:"q" long:"quiet" description:"quiet mode, no progress bars"`
	Debug          bool              `short:"d" long:"debug" description:"enable debug to stderr"`
	Version        bool              `short:"v" long:"version" description:"show version"`
	Https          struct {
		CertsFileName      string `short:"c" long:"certs-file" value-name:"certs.crt" description:"root certificates to use when verifying server certificates"`
		InsecureSkipVerify bool   `long:"no-check-cert" description:"don't verify the server's certificate chain and host name"`
	} `group:"Https Options"`
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
	loggers [LEVELS]*log.Logger
}

func (cmd Cmd) Exit(err error) (status int) {
	defer func() {
		if status == 0 {
			return
		}
		if cmd.loggers[DEBUG] != nil {
			// if there is stack trace available, +v will include it
			cmd.loggers[DEBUG].Printf("Exit error: %+v", err)
		}
	}()
	switch cause := errors.Cause(err).(type) {
	case nil:
		return 0
	case *flags.Error:
		if cause.Type == flags.ErrHelp {
			// cmd invoked with --help switch
			return 0
		}
		cmd.parser.WriteHelp(cmd.Err)
		return 2
	case ExpectedError:
		if cause == ErrBadInvariant {
			log.Default().Println(err.Error())
		} else {
			cmd.loggers[ERRO].Println(err.Error())
		}
		return 1
	default:
		cmd.loggers[ERRO].Println(err.Error())
		return 3
	}
}

func (cmd *Cmd) Run(args []string, version, commit string) (err error) {
	defer func() {
		// just add method name, without stack trace at the point
		err = errors.WithMessage(err, "run")
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
			pass, err := term.ReadPassword(0)
			if err != nil {
				return err
			}
			if cmd.Ctx.Err() != nil && context.Cause(cmd.Ctx) == ErrCanceledByUser {
				return ErrCanceledByUser
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
	rtBuilder := newRoundTripperBuilder(tlsConfig)

	cmd.opt.HeaderMap[hUserAgentKey] = userAgents[cmd.opt.UserAgent]
	cmd.patcher = makeReqPatcher(userinfo, cmd.opt.HeaderMap)

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
		return err
	}
	client := &http.Client{
		Transport: rtBuilder.pool(cmd.opt.Parts > 1).build(),
		Jar:       jar,
		CheckRedirect: func(_ *http.Request, via []*http.Request) error {
			max := int(cmd.opt.MaxRedirect)
			if max != 0 && len(via) > max {
				return errors.WithMessage(ErrMaxRedirect, "Stopping")
			}
			return http.ErrUseLastResponse
		},
	}
	session, err := cmd.getState(client)
	if err != nil {
		return err
	}
	session.summary(cmd.loggers)
	if cmd.opt.Parts == 0 {
		return nil
	}

	cmd.loggers[INFO].Printf("Saving to: %q", session.OutputName)

	if session.restored {
		cmd.patcher = makeReqPatcher(userinfo, session.HeaderMap)
	}

	var doneCount uint32
	single := len(session.Parts) == 1
	progress := mpb.NewWithContext(cmd.Ctx,
		mpb.WithDebugOutput(cmd.getErr()),
		mpb.WithOutput(cmd.getOut()),
		mpb.WithRefreshRate(refreshRate*time.Millisecond),
		mpb.WithWidth(64),
	)
	incrTotalBar, cancelTotalBar, err := cmd.initTotalBar(
		&doneCount,
		session,
		progress,
		single || cmd.opt.Quiet,
	)
	if err != nil {
		return err
	}
	stateHandler := cmd.makeStateHandler(session, progress)
	defer stateHandler(false)

	var eg errgroup.Group
	var recoverHandler sync.Once
	timeout := cmd.getTimeout()
	sleep := time.Duration(cmd.opt.SpeedLimit*60) * time.Millisecond

	statusOK := new(http200Context)
	statusOK.first = make(chan int)
	statusOK.ctx, statusOK.cancel = context.WithCancel(cmd.Ctx)
	defer statusOK.cancel()

	cancelMap := make(map[int]func())

	client.CheckRedirect = nil
	rtBuilder = nil

	for i, p := range session.Parts {
		if p.Written != 0 && p.isDone() {
			atomic.AddUint32(&doneCount, 1)
			continue
		}
		ctx, cancel := context.WithCancel(cmd.Ctx)
		cancelMap[i+1] = cancel
		p.ctx = ctx
		p.client = client
		p.statusOK = statusOK
		p.name = fmt.Sprintf("P%02d", i+1)
		p.order = i + 1
		p.single = single
		p.progress = progress
		p.incrTotalBar = incrTotalBar
		p.patcher = cmd.patcher
		p.debugWriter = cmd.getErr()
		p := p // https://golang.org/doc/faq#closures_and_goroutines
		eg.Go(func() error {
			defer func() {
				if e := recover(); e != nil {
					recoverHandler.Do(func() {
						for _, cancel := range cancelMap {
							cancel()
						}
						cancelTotalBar(false)
						stateHandler(true)
					})
					panic(fmt.Sprintf("%s panic: %v", p.name, e)) // https://go.dev/play/p/55nmnsXyfSA
				}
				if p.isDone() {
					atomic.AddUint32(&doneCount, 1)
				}
				cancel()
			}()
			maxTry := cmd.opt.MaxRetry
			return p.download(session.location, session.OutputName, timeout, sleep, maxTry)
		})
	}

	select {
	case id := <-statusOK.first:
		delete(cancelMap, id)
		for _, cancel := range cancelMap {
			cancel()
		}
		cancelTotalBar(true)
		single = true
		cmd.loggers[DEBUG].Printf("P%02d got http status 200", id)
	case <-statusOK.ctx.Done():
	}

	err = eg.Wait()
	if err != nil {
		cancelTotalBar(false)
		return err
	}

	if !single {
		err = session.concatenateParts(progress)
		if err != nil {
			return err
		}
	}
	if cmd.opt.SessionName != "" {
		return os.Remove(cmd.opt.SessionName)
	}
	return nil
}

func (cmd Cmd) makeStateHandler(session *Session, progress *mpb.Progress) func(bool) {
	pTotal := session.totalWritten()
	start := time.Now()
	return func(isPanic bool) {
		log := func() {}
		defer func() {
			progress.Wait()
			fmt.Fprintln(cmd.getOut())
			log()
		}()
		total := session.totalWritten()
		if session.isResumable() && total != session.ContentLength {
			if total-pTotal != 0 { // if some bytes were written
				session.Elapsed += time.Since(start)
				var name string
				if isPanic {
					name = session.OutputName + ".panic"
				} else {
					name = session.OutputName + ".json"
				}
				err := session.dumpState(name)
				if err != nil {
					log = func() {
						cmd.loggers[ERRO].Println(err.Error())
					}
				} else {
					log = func() {
						cmd.loggers[INFO].Printf("Session state saved to %q", name)
					}
				}
			}
		} else {
			log = func() {
				cmd.loggers[INFO].Printf("%q saved [%d/%d]", session.OutputName, session.ContentLength, total)
			}
		}
	}
}

func (cmd Cmd) getTLSConfig() (*tls.Config, error) {
	if cmd.opt.Https.CertsFileName != "" {
		buf, err := os.ReadFile(cmd.opt.Https.CertsFileName)
		if err != nil {
			return nil, err
		}
		pool, err := x509.SystemCertPool()
		if err != nil {
			return nil, err
		}
		if ok := pool.AppendCertsFromPEM(buf); !ok {
			return nil, errors.Errorf("bad cert file %q", cmd.opt.Https.CertsFileName)
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

func (cmd Cmd) getState(client *http.Client) (*Session, error) {
	setCookies := func(jar http.CookieJar, headers map[string]string, location string) error {
		cookies, err := parseCookies(headers)
		if err != nil {
			return err
		}
		if len(cookies) == 0 {
			return nil
		}
		u, err := url.Parse(location)
		if err != nil {
			return err
		}
		jar.SetCookies(u, cookies)
		return nil
	}
	var scratch, restored *Session
	for {
		switch {
		case cmd.opt.SessionName != "":
			restored = new(Session)
			err := restored.loadState(cmd.opt.SessionName)
			if err != nil {
				return nil, err
			}
			err = restored.checkSizeOfEachPart()
			if err != nil {
				return nil, err
			}
			err = setCookies(client.Jar, restored.HeaderMap, restored.URL)
			if err != nil {
				return nil, err
			}
			switch {
			case scratch == nil && restored.Redirected:
				scratch, err = cmd.follow(client, restored.URL)
				if err != nil {
					return nil, err
				}
				fallthrough
			case scratch != nil:
				err = restored.checkContentSums(*scratch)
				if err != nil {
					return nil, err
				}
				restored.location = scratch.location
			default:
				restored.location = restored.URL
			}
			restored.restored = true
			cmd.loggers[DEBUG].Printf("Session restored from: %q", cmd.opt.SessionName)
			return restored, nil
		case cmd.opt.Positional.Location != "":
			err := setCookies(client.Jar, cmd.opt.HeaderMap, cmd.opt.Positional.Location)
			if err != nil {
				return nil, err
			}
			scratch, err = cmd.follow(client, cmd.opt.Positional.Location)
			if err != nil {
				return nil, err
			}
			state := scratch.OutputName + ".json"
			if _, err := os.Stat(state); err != nil {
				if errors.Is(err, os.ErrNotExist) {
					if cmd.opt.Parts == 0 {
						return scratch, nil
					}
					exist, err := scratch.isOutputFileExist()
					if err != nil {
						return nil, err
					}
					if exist {
						err = cmd.overwriteIfConfirmed(scratch.OutputName)
						if err != nil {
							return nil, err
						}
					}
					err = scratch.calcParts(cmd.opt.Parts)
					if err != nil {
						return nil, err
					}
					scratch.HeaderMap = cmd.opt.HeaderMap
					return scratch, nil
				}
				return nil, err
			}
			cmd.loggers[DEBUG].Printf("Reusing existing state: %q", state)
			cmd.opt.SessionName = state
		}
	}
}

func (cmd Cmd) follow(client *http.Client, rawURL string) (session *Session, err error) {
	defer func() {
		err = errors.WithMessage(err, "follow")
	}()

	var redirected bool
	defer func() {
		if redirected {
			client.CloseIdleConnections()
		}
	}()

	location := rawURL
	timeout := cmd.getTimeout()
	template := "GET:R%02d"

	err = backoff.RetryWithContext(cmd.Ctx, exponential.New(exponential.WithBaseDelay(500*time.Millisecond)),
		func(attempt uint, _ func()) (retry bool, err error) {
			ctx, cancel := context.WithTimeout(cmd.Ctx, timeout)
			defer func() {
				if timeout < maxTimeout*time.Second {
					timeout += 5 * time.Second
				}
				cancel()
			}()
			for {
				getR := fmt.Sprintf(template, attempt)
				cmd.loggers[INFO].Println(getR, location)
				cmd.loggers[DEBUG].Println(getR, location)

				req, err := http.NewRequest(http.MethodGet, location, nil)
				if err != nil {
					return false, err
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
						return false, errors.WithMessage(ErrMaxRetry, "Stopping")
					}
					return !errors.Is(err, ErrMaxRedirect), err
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
						return false, err
					}
					location = loc.String()
					if resp.Body != nil {
						resp.Body.Close()
					}
					continue
				}

				if resp.StatusCode != http.StatusOK {
					cmd.loggers[WARN].Println("HTTP response:", resp.Status)
					err := errors.Wrap(BadHttpStatus(resp.StatusCode), resp.Status)
					if isServerError(resp.StatusCode) { // server error may be temporary
						return attempt != cmd.opt.MaxRetry, err
					}
					return false, err
				}

				cmd.loggers[INFO].Println("HTTP response:", resp.Status)

				name := cmd.opt.OutputName
				for i := 0; name == ""; i++ {
					switch i {
					case 0:
						name = parseContentDisposition(resp.Header.Get(hContentDisposition))
					case 1:
						if nURL, err := url.Parse(location); err != nil {
							name = location
						} else {
							nURL.RawQuery = ""
							name, err = url.QueryUnescape(nURL.String())
							if err != nil {
								name = nURL.String()
							}
						}
						name = filepath.Base(name)
					default:
						name = "unknown"
					}
				}

				session = &Session{
					location:      location,
					URL:           rawURL,
					OutputName:    name,
					ContentMD5:    resp.Header.Get(hContentMD5),
					AcceptRanges:  resp.Header.Get(hAcceptRanges),
					ContentType:   resp.Header.Get(hContentType),
					StatusCode:    resp.StatusCode,
					ContentLength: resp.ContentLength,
					Redirected:    redirected,
				}

				return false, resp.Body.Close()
			}
		})
	return session, err
}

func (cmd Cmd) initTotalBar(
	doneCount *uint32,
	session *Session,
	progress *mpb.Progress,
	dummy bool,
) (func(int), func(bool), error) {
	if dummy {
		return func(int) {}, func(bool) {}, nil
	}
	return session.runTotalBar(cmd.Ctx, doneCount, progress)
}

func (cmd Cmd) overwriteIfConfirmed(name string) error {
	if cmd.opt.ForceOverwrite {
		cmd.loggers[DEBUG].Printf("Removing existing: %q", name)
		return os.Remove(name)
	}
	var answer rune
	fmt.Fprintf(cmd.Err, "File %q already exists, overwrite? [Y/n] ", name)
	if _, err := fmt.Scanf("%c", &answer); err != nil {
		return err
	}
	switch answer {
	case '\n', 'y', 'Y':
		if cmd.Ctx.Err() != nil && context.Cause(cmd.Ctx) == ErrCanceledByUser {
			return ErrCanceledByUser
		}
		cmd.loggers[DEBUG].Printf("Removing existing: %q", name)
		return os.Remove(name)
	}
	return nil
}

func (cmd Cmd) getTimeout() time.Duration {
	if cmd.opt.Timeout == 0 {
		return 15 * time.Second
	}
	return time.Duration(cmd.opt.Timeout) * time.Second
}

func (cmd Cmd) getOut() io.Writer {
	if cmd.opt.Quiet {
		return io.Discard
	}
	return cmd.Out
}

func (cmd Cmd) getErr() io.Writer {
	if cmd.opt.Debug {
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

func unwrapOrErr(err error) error {
	if e := errors.Unwrap(err); e != nil {
		return e
	}
	return err
}

func firstErr(errors ...error) error {
	for _, err := range errors {
		if err != nil {
			return err
		}
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

func parseContentDisposition(input string) string {
	groups := reContentDisposition.FindAllStringSubmatch(input, -1)
	for _, group := range groups {
		if group[2] != "" {
			return group[2]
		}
		split := strings.Split(group[1], "'")
		if len(split) == 3 && strings.ToLower(split[0]) == "utf-8" {
			unescaped, _ := url.QueryUnescape(split[2])
			return unescaped
		}
		if split[0] != `""` {
			return split[0]
		}
	}
	return ""
}

func parseCookies(headers map[string]string) ([]*http.Cookie, error) {
	var cookies []*http.Cookie
	if hc, ok := headers[hCookie]; ok {
		for _, cookie := range strings.Split(hc, "; ") {
			k, v, ok := strings.Cut(cookie, "=")
			if !ok {
				continue
			}
			cookies = append(cookies, &http.Cookie{Name: k, Value: v})
		}
	}
	return cookies, nil
}

func isRedirect(status int) bool {
	return status > 299 && status < 400
}

func isServerError(status int) bool {
	return status > 499 && status < 600
}

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

type HttpError int

func (e HttpError) Error() string {
	return fmt.Sprintf("HTTP error: %d", int(e))
}

const (
	ErrBadInvariant   = ExpectedError("Bad invariant")
	ErrCanceledByUser = ExpectedError("Canceled by user")
	ErrMaxRedirect    = ExpectedError("Max redirects")
	ErrMaxRetry       = ExpectedError("Max retries")
)

const (
	cmdName     = "getparty"
	projectHome = "https://github.com/vbauerster/getparty"

	maxTimeout          = 180
	maxRedirects        = 10
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
	userAgents           map[string]string
	reContentDisposition = regexp.MustCompile(`filename[^;\n=]*=(['"](.*?)['"]|[^;\n]*)`) // https://regex101.com/r/N4AovD/3
)

func init() {
	userAgents = map[string]string{
		"chrome":  "Mozilla/5.0 (X11; Linux x86_64) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/113.0.0.0 Safari/537.36",
		"firefox": "Mozilla/5.0 (X11; Linux x86_64; rv:109.0) Gecko/20100101 Firefox/112.0",
		"safari":  "Mozilla/5.0 (Macintosh; Intel Mac OS X 11_4) AppleWebKit/605.1.15 (KHTML, like Gecko) Version/14.1 Safari/605.1.15",
		"edge":    "Mozilla/5.0 (Macintosh; Intel Mac OS X 11_4) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/91.0.4472.101 Safari/537.36 Edg/91.0.864.37",
	}
	userAgents[""] = userAgents["chrome"]
}

// Options struct, represents cmd line options
type Options struct {
	Parts              uint              `short:"p" long:"parts" value-name:"n" default:"1" description:"number of parts"`
	MaxRetry           uint              `short:"r" long:"max-retry" value-name:"n" default:"10" description:"max retry per each part, 0 for infinite"`
	Timeout            uint              `short:"t" long:"timeout" value-name:"sec" default:"15" description:"context timeout"`
	SpeedLimit         uint              `short:"l" long:"speed-limit" value-name:"n" description:"speed limit gauge, value from 1 to 10 inclusive"`
	OutFileName        string            `short:"o" long:"output" value-name:"filename" description:"user defined output"`
	JSONFileName       string            `short:"s" long:"session" value-name:"session.json" description:"path to saved session file (optional)"`
	UserAgent          string            `short:"a" long:"user-agent" choice:"chrome" choice:"firefox" choice:"safari" choice:"edge" choice:"getparty" description:"User-Agent header (default: chrome)"`
	BestMirror         []bool            `short:"b" long:"best-mirror" description:"pickup best mirror, repeat n times to list top n"`
	Quiet              bool              `short:"q" long:"quiet" description:"quiet mode, no progress bars"`
	ForceOverwrite     bool              `short:"f" long:"force" description:"overwrite existing file silently"`
	AuthUser           string            `short:"u" long:"username" description:"basic http auth username"`
	AuthPass           string            `long:"password" description:"basic http auth password"`
	HeaderMap          map[string]string `short:"H" long:"header" value-name:"key:value" description:"http header, can be specified more than once"`
	InsecureSkipVerify bool              `long:"no-check-cert" description:"don't validate the server's certificate"`
	CertsFileName      string            `short:"c" long:"certs-file" value-name:"certs.crt" description:"root certificates to use when verifying server certificates"`
	Debug              bool              `long:"debug" description:"enable debug to stderr"`
	Version            bool              `short:"v" long:"version" description:"show version"`
}

type Cmd struct {
	Ctx     context.Context
	Out     io.Writer
	Err     io.Writer
	options *Options
	parser  *flags.Parser
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
			log.Default().Println(err)
		} else {
			cmd.loggers[ERRO].Println(err)
		}
		return 1
	default:
		cmd.loggers[ERRO].Println(err)
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

	cmd.options = new(Options)
	cmd.parser = flags.NewParser(cmd.options, flags.Default)
	cmd.parser.Usage = "[OPTIONS] url"
	args, err = cmd.parser.ParseArgs(args)
	if err != nil {
		return err
	}

	userAgents[cmdName] = fmt.Sprintf("%s/%s", cmdName, version)

	if cmd.options.Version {
		fmt.Fprintf(cmd.Out, "%s (%.7s) (%s)\n", userAgents[cmdName], commit, runtime.Version())
		fmt.Fprintf(cmd.Out, "Project home: %s\n", projectHome)
		return nil
	}

	var userinfo *url.Userinfo
	if cmd.options.AuthUser != "" {
		if cmd.options.AuthPass == "" {
			fmt.Fprint(cmd.Out, "Enter password: ")
			pass, err := term.ReadPassword(0)
			if firstNonNil(err, cmd.Ctx.Err()) != nil {
				return err
			}
			cmd.options.AuthPass = string(pass)
			fmt.Fprintln(cmd.Out)
		}
		userinfo = url.UserPassword(cmd.options.AuthUser, cmd.options.AuthPass)
	}

	tlsConfig, err := cmd.getTLSConfig()
	if err != nil {
		return err
	}

	cmd.Out = cmd.getOut()
	cmd.Err = cmd.getErr()
	cmd.initLoggers()

	if len(cmd.options.BestMirror) != 0 {
		transport := newRoundTripperBuilder(false).withTLSConfig(tlsConfig).build()
		top, err := cmd.bestMirror(4, transport, args)
		if err != nil {
			return err
		}
		if len(top) == 1 {
			args = top
		} else {
			return nil
		}
	}

	// All users of cookiejar should import "golang.org/x/net/publicsuffix"
	jar, err := cookiejar.New(&cookiejar.Options{PublicSuffixList: publicsuffix.List})
	if err != nil {
		return err
	}
	if _, ok := cmd.options.HeaderMap[hUserAgentKey]; !ok {
		cmd.options.HeaderMap[hUserAgentKey] = userAgents[cmd.options.UserAgent]
	}
	transport := newRoundTripperBuilder(cmd.options.Parts != 0).withTLSConfig(tlsConfig).build()
	client := &http.Client{
		Transport: transport,
		Jar:       jar,
	}
	session, err := cmd.getState(userinfo, client, args)
	if err = firstNonNil(err, cmd.Ctx.Err()); err != nil {
		return err
	}
	session.summary(cmd.loggers[INFO])
	if cmd.options.Parts == 0 {
		return nil
	}

	var doneCount uint32
	var eg errgroup.Group
	var onceSessionHandle sync.Once
	ctx, cancel := context.WithCancel(cmd.Ctx)
	defer cancel()
	progress := mpb.NewWithContext(ctx,
		mpb.WithDebugOutput(cmd.Err),
		mpb.WithOutput(cmd.Out),
		mpb.WithRefreshRate(refreshRate*time.Millisecond),
		mpb.WithWidth(64),
	)
	totalBarIncr, totalCancel, err := session.makeTotalBar(ctx,
		progress,
		&doneCount,
		cmd.options.Quiet,
	)
	if err != nil {
		return err
	}
	sessionHandle := cmd.makeSessionHandler(session, progress)
	defer sessionHandle()

	patcher := makeReqPatcher(userinfo, session.HeaderMap, true)
	timeout := cmd.getTimeout()
	sleep := cmd.getSleep()

	for i, p := range session.Parts {
		if p.isDone() {
			atomic.AddUint32(&doneCount, 1)
			continue
		}
		p.ctx = ctx
		p.order = i + 1
		p.name = fmt.Sprintf("P%02d", p.order)
		p.maxTry = cmd.options.MaxRetry
		p.single = len(session.Parts) == 1
		p.progress = progress
		p.totalBarIncr = totalBarIncr
		p.dlogger = log.New(cmd.Err, fmt.Sprintf("[%s:R%%02d] ", p.name), log.LstdFlags)
		p.reqPatcher = patcher
		p := p // https://golang.org/doc/faq#closures_and_goroutines
		client := &http.Client{
			Transport: transport,
			Jar:       jar,
		}
		eg.Go(func() error {
			defer func() {
				if e := recover(); e != nil {
					cancel()
					onceSessionHandle.Do(sessionHandle)
					panic(fmt.Sprintf("%s panic: %v", p.name, e)) // https://go.dev/play/p/55nmnsXyfSA
				}
				switch {
				case p.isDone():
					atomic.AddUint32(&doneCount, 1)
				case p.Skip:
					totalCancel(true) // totalCancel is idempotent
				}
			}()
			return p.download(session.location, client, timeout, sleep)
		})
	}

	cmd.parser = nil

	err = firstNonNil(eg.Wait(), ctx.Err())
	if err != nil {
		totalCancel(false)
		return err
	}

	err = session.concatenateParts(progress, cmd.loggers[DEBUG])
	if err != nil {
		return err
	}
	if cmd.options.JSONFileName != "" {
		return os.Remove(cmd.options.JSONFileName)
	}
	return nil
}

func (cmd Cmd) getTLSConfig() (*tls.Config, error) {
	var config *tls.Config
	if cmd.options.InsecureSkipVerify {
		config = &tls.Config{InsecureSkipVerify: true}
	} else if cmd.options.CertsFileName != "" {
		buf, err := os.ReadFile(cmd.options.CertsFileName)
		if err != nil {
			return nil, err
		}
		pool, err := x509.SystemCertPool()
		if err != nil {
			return nil, err
		}
		if ok := pool.AppendCertsFromPEM(buf); !ok {
			return nil, errors.Errorf("bad cert file %q", cmd.options.CertsFileName)
		}
		config = &tls.Config{RootCAs: pool}
	}
	return config, nil
}

func (cmd Cmd) makeSessionHandler(session *Session, progress *mpb.Progress) func() {
	pTotal := session.totalWritten()
	start := time.Now()
	return func() {
		log := func() {}
		defer func() {
			progress.Wait()
			fmt.Fprintln(cmd.Out)
			log()
		}()
		total := session.totalWritten()
		if session.isResumable() && total != session.ContentLength {
			if total-pTotal != 0 { // if some bytes were written
				session.Elapsed += time.Since(start)
				session.dropSkipped()
				name := session.OutputFileName + ".json"
				err := session.dumpState(name)
				if err != nil {
					log = func() {
						cmd.loggers[ERRO].Println(err)
					}
				} else {
					log = func() {
						cmd.loggers[INFO].Printf("Session state saved to %q", name)
					}
				}
			}
		} else {
			log = func() {
				cmd.loggers[INFO].Printf("%q saved [%d/%d]", session.OutputFileName, session.ContentLength, total)
			}
		}
	}
}

func (cmd Cmd) getState(
	userinfo *url.Userinfo,
	client *http.Client,
	args []string,
) (*Session, error) {
	setJarCookies := func(rawURL string, headers map[string]string, jar http.CookieJar) error {
		cookies, err := parseCookies(headers)
		if err != nil {
			return err
		}
		if len(cookies) == 0 {
			return nil
		}
		u, err := url.Parse(rawURL)
		if err != nil {
			return err
		}
		jar.SetCookies(u, cookies)
		return nil
	}
	client.CheckRedirect = func(_ *http.Request, via []*http.Request) error {
		if len(via) >= maxRedirects {
			return errors.WithMessagef(ErrMaxRedirect, "stopped after %d redirects", maxRedirects)
		}
		return http.ErrUseLastResponse
	}
	var scratch, restored *Session
	for {
		switch {
		case cmd.options.JSONFileName != "":
			restored = new(Session)
			err := restored.loadState(cmd.options.JSONFileName)
			if err != nil {
				return nil, err
			}
			err = restored.checkSizeOfEachPart()
			if err != nil {
				return nil, err
			}
			err = setJarCookies(restored.URL, restored.HeaderMap, client.Jar)
			if err != nil {
				return nil, err
			}
			if cmd.options.UserAgent != "" || restored.HeaderMap[hUserAgentKey] == "" {
				restored.HeaderMap[hUserAgentKey] = userAgents[cmd.options.UserAgent]
			}
			switch {
			case scratch == nil && restored.Redirected:
				scratch, err = cmd.follow(restored.URL, client, makeReqPatcher(userinfo, restored.HeaderMap, true))
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
			cmd.loggers[DEBUG].Printf("Session restored from: %q", cmd.options.JSONFileName)
			return restored, nil
		case len(args) != 0:
			err := setJarCookies(args[0], cmd.options.HeaderMap, client.Jar)
			if err != nil {
				return nil, err
			}
			scratch, err = cmd.follow(args[0], client, makeReqPatcher(userinfo, cmd.options.HeaderMap, true))
			if err != nil {
				return nil, err
			}
			state := scratch.OutputFileName + ".json"
			if _, err := os.Stat(state); err == nil {
				cmd.options.JSONFileName = state
			} else if errors.Is(err, os.ErrNotExist) {
				if cmd.options.Parts != 0 {
					exist, err := scratch.isOutputFileExist()
					if err != nil {
						return nil, err
					}
					if exist {
						err = cmd.overwriteIfConfirmed(scratch.OutputFileName)
						if err != nil {
							return nil, err
						}
					}
					err = scratch.calcParts(cmd.options.Parts)
					if err != nil {
						return nil, err
					}
				}
				scratch.HeaderMap = cmd.options.HeaderMap
				return scratch, nil
			} else {
				return nil, err
			}
		default:
			return nil, new(flags.Error)
		}
	}
}

func (cmd Cmd) follow(
	rawURL string,
	client *http.Client,
	reqPatcher func(*http.Request),
) (session *Session, err error) {
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
				req, err := http.NewRequest(http.MethodGet, location, nil)
				if err != nil {
					return false, err
				}

				reqPatcher(req)

				for k, v := range req.Header {
					cmd.loggers[DEBUG].Printf("%s: %v", k, v)
				}

				if attempt == 0 {
					cmd.loggers[INFO].Printf("Get %q", location)
				} else {
					cmd.loggers[INFO].Printf("Get:R%02d %q", attempt, location)
				}

				resp, err := client.Do(req.WithContext(ctx))
				if err != nil {
					if e := errors.Unwrap(err); e != nil {
						cmd.loggers[WARN].Println(e.Error())
					} else {
						cmd.loggers[WARN].Println(err.Error())
					}
					if attempt != 0 && attempt == cmd.options.MaxRetry {
						return false, errors.Wrap(ErrMaxRetry, err.Error())
					}
					return true, err
				}

				if cookies := client.Jar.Cookies(req.URL); len(cookies) != 0 {
					cmd.loggers[DEBUG].Println("CookieJar:")
					for _, cookie := range cookies {
						cmd.loggers[DEBUG].Printf("  %q", cookie)
					}
				}

				if isRedirect(resp.StatusCode) {
					cmd.loggers[INFO].Printf("HTTP response: %s", resp.Status)
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
					cmd.loggers[WARN].Printf("HTTP response: %s", resp.Status)
					err = HttpError(resp.StatusCode)
					if isServerError(resp.StatusCode) { // server error may be temporary
						return attempt != cmd.options.MaxRetry, err
					}
					return false, err
				}

				cmd.loggers[INFO].Printf("HTTP response: %s", resp.Status)

				name := cmd.options.OutFileName
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
					location:       location,
					URL:            rawURL,
					OutputFileName: name,
					ContentMD5:     resp.Header.Get(hContentMD5),
					AcceptRanges:   resp.Header.Get(hAcceptRanges),
					ContentType:    resp.Header.Get(hContentType),
					StatusCode:     resp.StatusCode,
					ContentLength:  resp.ContentLength,
					Redirected:     redirected,
				}

				resp.Body.Close()
				return false, nil
			}
		})
	return session, err
}

func (cmd Cmd) overwriteIfConfirmed(name string) error {
	if cmd.options.ForceOverwrite {
		return os.Remove(name)
	}
	var answer rune
	fmt.Fprintf(os.Stderr, "File %q already exists, overwrite? [Y/n] ", name)
	if _, err := fmt.Scanf("%c", &answer); err != nil {
		return err
	}
	switch answer {
	case '\n', 'y', 'Y':
		if cmd.Ctx.Err() == nil {
			return os.Remove(name)
		}
		fallthrough
	default:
		return ErrCanceledByUser
	}
}

func (cmd Cmd) getTimeout() time.Duration {
	if cmd.options.Timeout == 0 {
		return 15 * time.Second
	}
	return time.Duration(cmd.options.Timeout) * time.Second
}

func (cmd Cmd) getSleep() time.Duration {
	switch l := cmd.options.SpeedLimit; l {
	case 1, 2, 3, 4, 5, 6, 7, 8, 9, 10:
		return time.Duration(l*50) * time.Millisecond
	default:
		return 0
	}
}

func (cmd Cmd) getOut() io.Writer {
	if cmd.options.Quiet {
		return io.Discard
	}
	return cmd.Out
}

func (cmd Cmd) getErr() io.Writer {
	if cmd.options.Debug {
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

func firstNonNil(errors ...error) error {
	for _, err := range errors {
		if err != nil {
			return err
		}
	}
	return nil
}

func makeReqPatcher(userinfo *url.Userinfo, headers map[string]string, skipCookie bool) func(*http.Request) {
	return func(req *http.Request) {
		if req == nil {
			return
		}
		if req.URL != nil {
			req.URL.User = userinfo
		}
		for k, v := range headers {
			if skipCookie && k == hCookie {
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

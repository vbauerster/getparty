package getparty

import (
	"bufio"
	"context"
	"crypto/tls"
	"crypto/x509"
	"encoding/json"
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
	"unicode"

	cleanhttp "github.com/hashicorp/go-cleanhttp"
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
	BestMirror         bool              `short:"b" long:"best-mirror" description:"pickup the fastest mirror"`
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
	Ctx       context.Context
	Out       io.Writer
	Err       io.Writer
	options   *Options
	parser    *flags.Parser
	userinfo  *url.Userinfo
	tlsConfig *tls.Config
	logger    *log.Logger
	dlogger   *log.Logger
}

func (cmd Cmd) Exit(err error) int {
	if cmd.dlogger != nil {
		// if there is stack trace available, +v will include it
		cmd.dlogger.Printf("Exit error: %+v", err)
	}
	switch e := errors.Cause(err).(type) {
	case nil:
		return 0
	case *flags.Error:
		if e.Type == flags.ErrHelp {
			return 0
		}
		cmd.parser.WriteHelp(cmd.Err)
		return 2
	case ExpectedError:
		cmd.logError(e)
		return 1
	default:
		cmd.logError(err)
		return 3
	}
}

func (cmd Cmd) logError(err error) {
	cmd.logger.SetPrefix("[ERRO] ")
	cmd.logger.Println(err.Error())
}

func (cmd *Cmd) Run(args []string, version, commit string) (err error) {
	defer func() {
		// just add method name, without stack trace at the point
		err = errors.WithMessage(err, "run")
	}()

	cmd.options = new(Options)
	cmd.parser = flags.NewParser(cmd.options, flags.Default)
	cmd.parser.Usage = "[OPTIONS] url"
	args, err = cmd.parser.ParseArgs(args)
	if err != nil {
		return err
	}

	if cmd.options.Quiet {
		cmd.Out = io.Discard
	}

	userAgents[cmdName] = fmt.Sprintf("%s/%s", cmdName, version)

	if cmd.options.Version {
		fmt.Fprintf(cmd.Out, "%s (%.7s) (%s)\n", userAgents[cmdName], commit, runtime.Version())
		fmt.Fprintf(cmd.Out, "Project home: %s\n", projectHome)
		return nil
	}

	cmd.logger = setupLogger(cmd.Out, "[INFO] ", cmd.options.Quiet)
	cmd.dlogger = setupLogger(cmd.Err, fmt.Sprintf("[%s] ", cmdName), !cmd.options.Debug)

	if cmd.options.AuthUser != "" {
		if cmd.options.AuthPass == "" {
			err = eitherError(cmd.readPassword(), cmd.Ctx.Err())
			if err != nil {
				return err
			}
			fmt.Fprintln(cmd.Out)
		}
		cmd.userinfo = url.UserPassword(cmd.options.AuthUser, cmd.options.AuthPass)
	}

	if cmd.options.Timeout == 0 {
		cmd.options.Timeout = 15
	}

	if _, ok := cmd.options.HeaderMap[hUserAgentKey]; !ok {
		cmd.options.HeaderMap[hUserAgentKey] = userAgents[cmd.options.UserAgent]
	}

	if cmd.options.InsecureSkipVerify {
		cmd.tlsConfig = &tls.Config{InsecureSkipVerify: true}
	} else if cmd.options.CertsFileName != "" {
		buf, err := os.ReadFile(cmd.options.CertsFileName)
		if err != nil {
			return err
		}
		pool, err := x509.SystemCertPool()
		if err != nil {
			return err
		}
		if ok := pool.AppendCertsFromPEM(buf); !ok {
			return errors.Errorf("bad cert file %q", cmd.options.CertsFileName)
		}
		cmd.tlsConfig = &tls.Config{RootCAs: pool}
	}

	if cmd.options.BestMirror {
		url, err := cmd.bestMirror(args, makeReqPatcher(cmd.options.HeaderMap, false))
		if err != nil {
			return err
		}
		args = append(args[:0], url)
	}

	// All users of cookiejar should import "golang.org/x/net/publicsuffix"
	jar, err := cookiejar.New(&cookiejar.Options{PublicSuffixList: publicsuffix.List})
	if err != nil {
		return err
	}
	transport := cmd.getTransport(cmd.options.Parts != 0)
	session, err := cmd.getState(args, transport, jar)
	if err = eitherError(err, cmd.Ctx.Err()); err != nil {
		return err
	}
	session.summary(cmd.logger)
	if cmd.options.Parts == 0 {
		return nil
	}

	var doneCount uint32
	var eg errgroup.Group
	ctx, cancel := context.WithCancel(cmd.Ctx)
	defer cancel()
	progress := mpb.NewWithContext(ctx,
		mpb.ContainerOptional(mpb.WithOutput(cmd.Out), !cmd.options.Quiet),
		mpb.ContainerOptional(mpb.WithOutput(nil), cmd.options.Quiet),
		mpb.ContainerOptional(mpb.WithDebugOutput(cmd.Err), cmd.options.Debug),
		mpb.WithRefreshRate(refreshRate*time.Millisecond),
		mpb.WithWidth(64),
	)
	totalEwmaInc, totalDrop := session.makeTotalBar(ctx, progress, &doneCount, cmd.options.Quiet)
	defer totalDrop()
	patcher := makeReqPatcher(session.HeaderMap, true)
	timeout := time.Duration(cmd.options.Timeout) * time.Second
	var sleep time.Duration
	switch l := cmd.options.SpeedLimit; l {
	case 1, 2, 3, 4, 5, 6, 7, 8, 9, 10:
		sleep = time.Duration(l*50) * time.Millisecond
	}
	sessionHandle := cmd.makeSessionHandler(session)
	defer sessionHandle()
	defer progress.Wait()

	var onceSessionHandle sync.Once
	for i, p := range session.Parts {
		if p.isDone() {
			atomic.AddUint32(&doneCount, 1)
			continue
		}
		p.ctx = ctx
		p.order = i + 1
		p.name = fmt.Sprintf("P%02d", p.order)
		p.maxTry = cmd.options.MaxRetry
		p.quiet = cmd.options.Quiet
		p.single = len(session.Parts) == 1
		p.progress = progress
		p.totalEwmaInc = totalEwmaInc
		p.dlogger = setupLogger(cmd.Err, fmt.Sprintf("[%s:R%%02d] ", p.name), !cmd.options.Debug)
		req, err := http.NewRequest(http.MethodGet, session.location, nil)
		if err != nil {
			cmd.logger.Fatalf("%s: %s", p.name, err.Error())
		}
		patcher(req, cmd.userinfo)
		p := p // https://golang.org/doc/faq#closures_and_goroutines
		eg.Go(func() error {
			defer func() {
				if e := recover(); e != nil {
					cancel()
					progress.Wait()
					onceSessionHandle.Do(sessionHandle)
					panic(fmt.Sprintf("%s panic: %v", p.name, e)) // https://go.dev/play/p/55nmnsXyfSA
				}
				switch {
				case p.isDone():
					atomic.AddUint32(&doneCount, 1)
				case p.Skip:
					cmd.dlogger.Print("Dropping total bar")
					totalDrop()
				}
			}()
			return p.download(&http.Client{
				Transport: transport,
				Jar:       jar,
			}, req, timeout, sleep)
		})
	}

	cmd.userinfo = nil
	cmd.tlsConfig = nil
	cmd.parser = nil

	err = eitherError(eg.Wait(), cmd.Ctx.Err())
	if err != nil {
		return err
	}

	err = session.concatenateParts(progress, cmd.dlogger)
	if err != nil {
		return err
	}
	if cmd.options.JSONFileName != "" {
		return os.Remove(cmd.options.JSONFileName)
	}
	return nil
}

func (cmd Cmd) makeSessionHandler(session *Session) func() {
	pTotal := session.totalWritten()
	start := time.Now()
	return func() {
		fmt.Fprintln(cmd.Out)
		total := session.totalWritten()
		if session.isResumable() && total != session.ContentLength {
			if total-pTotal != 0 { // if some bytes were written
				session.Elapsed += time.Since(start)
				session.dropSkipped()
				cmd.dumpState(session)
			}
		} else {
			cmd.logger.Printf("%q saved [%d/%d]", session.OutputFileName, session.ContentLength, total)
		}
	}
}

func (cmd Cmd) getState(args []string, transport http.RoundTripper, jar http.CookieJar) (*Session, error) {
	setJarCookies := func(headers map[string]string, rawURL string) error {
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
			err = setJarCookies(restored.HeaderMap, restored.URL)
			if err != nil {
				return nil, err
			}
			if cmd.options.UserAgent != "" || restored.HeaderMap[hUserAgentKey] == "" {
				restored.HeaderMap[hUserAgentKey] = userAgents[cmd.options.UserAgent]
			}
			switch {
			case scratch == nil && restored.Redirected:
				scratch, err = cmd.follow(restored.URL, transport, jar, makeReqPatcher(restored.HeaderMap, true))
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
			return restored, nil
		case len(args) != 0:
			err := setJarCookies(cmd.options.HeaderMap, args[0])
			if err != nil {
				return nil, err
			}
			scratch, err = cmd.follow(args[0], transport, jar, makeReqPatcher(cmd.options.HeaderMap, true))
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
	transport http.RoundTripper,
	jar http.CookieJar,
	reqPatcher func(*http.Request, *url.Userinfo),
) (session *Session, err error) {
	defer func() {
		err = errors.WithMessage(err, "follow")
	}()

	client := &http.Client{
		Transport: transport,
		Jar:       jar,
		CheckRedirect: func(_ *http.Request, via []*http.Request) error {
			if len(via) > maxRedirects {
				return errors.WithMessagef(ErrMaxRedirect, "stopped after %d redirects", maxRedirects)
			}
			return http.ErrUseLastResponse
		},
	}
	var redirected bool
	defer func() {
		if redirected {
			client.CloseIdleConnections()
		}
	}()

	location := rawURL
	timeout := time.Duration(cmd.options.Timeout) * time.Second

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

				reqPatcher(req, cmd.userinfo)

				for k, v := range req.Header {
					cmd.dlogger.Printf("%s: %v", k, v)
				}

				if attempt == 0 {
					cmd.logger.Printf("Get %q", location)
				} else {
					cmd.logger.Printf("Get:R%02d %q", attempt, location)
				}

				resp, err := client.Do(req.WithContext(ctx))
				if err != nil {
					prefix := cmd.logger.Prefix()
					cmd.logger.SetPrefix("[WARN] ")
					if e := errors.Unwrap(err); e != nil {
						cmd.logger.Println(e.Error())
					} else {
						cmd.logger.Println(err.Error())
					}
					cmd.logger.SetPrefix(prefix)
					if attempt != 0 && attempt == cmd.options.MaxRetry {
						return false, errors.Wrap(ErrMaxRetry, err.Error())
					}
					return true, err
				}

				if cookies := jar.Cookies(req.URL); len(cookies) != 0 {
					cmd.dlogger.Println("CookieJar:")
					for _, cookie := range cookies {
						cmd.dlogger.Printf("  %q", cookie)
					}
				}

				if isRedirect(resp.StatusCode) {
					cmd.logger.Printf("HTTP response: %s", resp.Status)
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
					prefix := cmd.logger.Prefix()
					cmd.logger.SetPrefix("[WARN] ")
					cmd.logger.Printf("HTTP response: %s", resp.Status)
					err = HttpError(resp.StatusCode)
					if isServerError(resp.StatusCode) { // server error may be temporary
						cmd.logger.SetPrefix(prefix)
						return attempt != cmd.options.MaxRetry, err
					}
					return false, err
				}

				cmd.logger.Printf("HTTP response: %s", resp.Status)

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

func (cmd Cmd) getTransport(pooled bool) (transport *http.Transport) {
	if pooled {
		transport = cleanhttp.DefaultPooledTransport()
	} else {
		transport = cleanhttp.DefaultTransport()
	}
	transport.TLSClientConfig = cmd.tlsConfig
	return transport
}

func (cmd Cmd) bestMirror(
	args []string,
	reqPatcher func(*http.Request, *url.Userinfo),
) (best string, err error) {
	defer func() {
		err = errors.WithMessage(err, "bestMirror")
	}()

	input := os.Stdin
	if len(args) != 0 {
		fd, err := os.Open(args[0])
		if err != nil {
			return "", err
		}
		defer fd.Close()
		input = fd
	}
	urls, err := readLines(input)
	if err != nil {
		return "", err
	}
	var wg1, wg2 sync.WaitGroup
	start := make(chan struct{})
	first := make(chan string, 1)
	client := &http.Client{
		Transport: cmd.getTransport(false),
	}

	for _, u := range urls {
		req, err := http.NewRequest(http.MethodGet, u, nil)
		if err != nil {
			cmd.dlogger.Printf("skipping %q: %s", u, err.Error())
			continue
		}
		cmd.dlogger.Printf("fetching: %q", u)
		reqPatcher(req, cmd.userinfo)
		u := u // https://golang.org/doc/faq#closures_and_goroutines
		wg1.Add(1)
		wg2.Add(1)
		go func() {
			defer wg2.Done()
			wg1.Done()
			<-start
			ctx, cancel := context.WithTimeout(cmd.Ctx, time.Duration(cmd.options.Timeout)*time.Second)
			defer cancel()
			resp, err := client.Do(req.WithContext(ctx))
			if err != nil {
				cmd.dlogger.Printf("fetch error: %s", err.Error())
				return
			}
			defer resp.Body.Close()
			switch resp.StatusCode {
			case http.StatusOK:
				select {
				case first <- u:
				default:
				}
			default:
				cmd.dlogger.Printf("fetch %q unexpected status: %s", u, resp.Status)
			}
		}()
	}
	wg1.Wait()
	close(start)
	wg2.Wait()
	close(first)
	best = <-first
	if best == "" {
		return "", errors.New("Best mirror not found")
	}
	cmd.dlogger.Printf("best mirror: %q", best)
	return best, nil
}

func (cmd *Cmd) readPassword() error {
	fmt.Fprint(cmd.Out, "Enter Password: ")
	bytePassword, err := term.ReadPassword(0)
	if err != nil {
		return err
	}
	cmd.options.AuthPass = string(bytePassword)
	return nil
}

func (cmd Cmd) overwriteIfConfirmed(name string) error {
	if cmd.options.ForceOverwrite {
		return os.Remove(name)
	}
	var answer rune
	fmt.Fprintf(cmd.Err, "File %q already exists, overwrite? [Y/n] ", name)
	if _, err := fmt.Scanf("%c", &answer); err != nil {
		return err
	}
	switch answer {
	case '\n', 'y', 'Y':
		if cmd.Ctx.Err() == nil {
			return os.Remove(name)
		}
	default:
		return ErrCanceledByUser
	}
	return nil
}

func (cmd Cmd) dumpState(session *Session) {
	var media io.Writer
	name := session.OutputFileName + ".json"
	f, err := os.Create(name)
	if err != nil {
		media = cmd.Err
		name = "stderr"
	} else {
		defer func() {
			if err := f.Close(); err != nil {
				cmd.logError(err)
			}
		}()
		media = f
	}
	err = json.NewEncoder(media).Encode(session)
	if err != nil {
		cmd.logError(err)
	} else {
		cmd.logger.Printf("Session state saved to %q", name)
	}
}

func eitherError(errors ...error) error {
	for _, err := range errors {
		if err != nil {
			return err
		}
	}
	return nil
}

func makeReqPatcher(headers map[string]string, skipCookie bool) func(*http.Request, *url.Userinfo) {
	return func(req *http.Request, userinfo *url.Userinfo) {
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

func readLines(r io.Reader) ([]string, error) {
	var lines []string
	scanner := bufio.NewScanner(r)
	for scanner.Scan() {
		line := scanner.Text()
		if len(line) == 0 || strings.HasPrefix(line, "#") {
			continue
		}
		line = strings.TrimFunc(line, func(r rune) bool {
			return !unicode.IsLetter(r) && !unicode.IsNumber(r)
		})
		lines = append(lines, line)
	}
	return lines, scanner.Err()
}

func setupLogger(out io.Writer, prefix string, discard bool) *log.Logger {
	if discard {
		// log.Logger optimizes for io.Discard
		// https://github.com/golang/go/blob/db3045b4be5b91cd42c3387dc550c89bbc2f7fb4/src/log/log_test.go#L183-L192
		out = io.Discard
	}
	return log.New(out, prefix, log.LstdFlags)
}

package getparty

import (
	"bufio"
	"context"
	"crypto/tls"
	"crypto/x509"
	"fmt"
	"io"
	"io/ioutil"
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
	"syscall"
	"time"

	cleanhttp "github.com/hashicorp/go-cleanhttp"
	flags "github.com/jessevdk/go-flags"
	"github.com/pkg/errors"
	"github.com/vbauerster/backoff"
	"github.com/vbauerster/backoff/exponential"
	"github.com/vbauerster/mpb/v7"
	"golang.org/x/crypto/ssh/terminal"
	"golang.org/x/net/publicsuffix"
	"golang.org/x/sync/errgroup"
)

type ExpectedError string

func (e ExpectedError) Error() string {
	return string(e)
}

type HttpError struct {
	StatusCode int
	Status     string
}

func (e HttpError) Error() string {
	return fmt.Sprintf("HTTP error: %s", e.Status)
}

const (
	ErrCanceledByUser = ExpectedError("Canceled by user")
	ErrMaxRedirects   = ExpectedError("Max redirects reached")
	ErrMaxRetry       = ExpectedError("Max retry reached")
)

const (
	cmdName     = "getparty"
	projectHome = "https://github.com/vbauerster/getparty"

	maxRedirects        = 10
	refreshRate         = 200
	hUserAgentKey       = "User-Agent"
	hContentDisposition = "Content-Disposition"
	hRange              = "Range"
	hCookie             = "Cookie"
)

// https://regex101.com/r/N4AovD/3
var reContentDisposition = regexp.MustCompile(`filename[^;\n=]*=(['"](.*?)['"]|[^;\n]*)`)

var userAgents = map[string]string{
	"chrome":  "Mozilla/5.0 (Macintosh; Intel Mac OS X 11_4) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/91.0.4472.101 Safari/537.36",
	"firefox": "Mozilla/5.0 (Macintosh; Intel Mac OS X 11.4; rv:89.0) Gecko/20100101 Firefox/89.0",
	"safari":  "Mozilla/5.0 (Macintosh; Intel Mac OS X 11_4) AppleWebKit/605.1.15 (KHTML, like Gecko) Version/14.1 Safari/605.1.15",
	"edge":    "Mozilla/5.0 (Macintosh; Intel Mac OS X 11_4) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/91.0.4472.101 Safari/537.36 Edg/91.0.864.37",
}

// Options struct, represents cmd line options
type Options struct {
	Parts              uint              `short:"p" long:"parts" value-name:"n" default:"2" description:"number of parts"`
	MaxRetry           uint              `short:"r" long:"max-retry" value-name:"n" default:"10" description:"max retry per each part, 0 for infinite"`
	Timeout            uint              `short:"t" long:"timeout" value-name:"sec" default:"15" description:"context timeout"`
	OutFileName        string            `short:"o" long:"output" value-name:"filename" description:"user defined output"`
	JSONFileName       string            `short:"c" long:"continue" value-name:"state.json" description:"resume download from the last session"`
	UserAgent          string            `short:"a" long:"user-agent" choice:"chrome" choice:"firefox" choice:"safari" choice:"edge" choice:"getparty" default:"chrome" description:"User-Agent header"`
	BestMirror         bool              `short:"b" long:"best-mirror" description:"pickup the fastest mirror"`
	Quiet              bool              `short:"q" long:"quiet" description:"quiet mode, no progress bars"`
	ForceOverwrite     bool              `short:"f" long:"force" description:"overwrite existing file silently"`
	AuthUser           string            `short:"u" long:"username" description:"basic http auth username"`
	AuthPass           string            `long:"password" description:"basic http auth password"`
	HeaderMap          map[string]string `short:"H" long:"header" value-name:"key:value" description:"arbitrary http header"`
	InsecureSkipVerify bool              `long:"no-check-cert" description:"don't validate the server's certificate"`
	CertsFileName      string            `long:"certs-file" value-name:"certs.crt" description:"root certificates to use when verifying server certificates"`
	Debug              bool              `long:"debug" description:"enable debug to stderr"`
	Version            bool              `long:"version" description:"show version"`
}

type Cmd struct {
	Ctx      context.Context
	Out      io.Writer
	Err      io.Writer
	userInfo *url.Userinfo
	options  *Options
	parser   *flags.Parser
	logger   *log.Logger
	dlogger  *log.Logger
}

func (cmd Cmd) Exit(err error) int {
	if cmd.Ctx.Err() == context.Canceled {
		// most probably user hit ^C, so mark as expected
		err = errors.WithMessage(ErrCanceledByUser, err.Error())
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
	case *url.Error:
		cmd.debugOrPrintErr(err, true)
		return cmd.Exit(e.Err)
	case ExpectedError:
		cmd.debugOrPrintErr(err, true)
		return 1
	default:
		cmd.debugOrPrintErr(err, false)
		return 3
	}
}

func (cmd Cmd) debugOrPrintErr(err error, expected bool) {
	var unexpected string
	if !expected {
		unexpected = "unexpected: "
	}
	if cmd.options.Debug {
		// if there is stack trace available, +v will include it
		cmd.dlogger.Printf("%s%+v", unexpected, err)
	} else {
		fmt.Fprintf(cmd.Err, "%s%s\n", unexpected, err.Error())
	}
}

func (cmd *Cmd) Run(args []string, version, commit string) (err error) {
	runtime.MemProfileRate = 0
	defer func() {
		// just add method name, without stack trace at the point
		err = errors.WithMessage(err, "run")
	}()
	userAgents[cmdName] = fmt.Sprintf("%s/%s", cmdName, version)
	cmd.options = new(Options)
	cmd.parser = flags.NewParser(cmd.options, flags.Default)
	cmd.parser.Name = cmdName
	cmd.parser.Usage = "[OPTIONS] url"

	args, err = cmd.parser.ParseArgs(args)
	if err != nil {
		return err
	}

	if cmd.options.Version {
		fmt.Fprintf(cmd.Out, "%s (%.7s) (%s)\n", userAgents[cmdName], commit, runtime.Version())
		fmt.Fprintf(cmd.Out, "Project home: %s\n", projectHome)
		return nil
	}

	if cmd.options.AuthUser != "" {
		if cmd.options.AuthPass == "" {
			cmd.options.AuthPass, err = cmd.readPassword()
			if err != nil {
				return err
			}
		}
		cmd.userInfo = url.UserPassword(cmd.options.AuthUser, cmd.options.AuthPass)
	}

	setupLogger := func(out io.Writer, prefix string, discard bool) *log.Logger {
		if discard {
			out = ioutil.Discard
		}
		return log.New(out, prefix, log.LstdFlags)
	}

	cmd.logger = setupLogger(cmd.Out, "", cmd.options.Quiet)
	cmd.dlogger = setupLogger(cmd.Err, fmt.Sprintf("[%s] ", cmdName), !cmd.options.Debug)

	var userUrl string
	var lastSession *Session

	switch {
	case cmd.options.JSONFileName != "":
		lastSession = new(Session)
		err := lastSession.loadState(cmd.options.JSONFileName)
		if err != nil {
			return err
		}
		userUrl = lastSession.Location
		cmd.options.HeaderMap = lastSession.HeaderMap
		cmd.options.OutFileName = lastSession.SuggestedFileName
	case cmd.options.BestMirror:
		var input io.Reader
		var rr []io.Reader
		for _, fn := range args {
			if fd, err := os.Open(fn); err == nil {
				rr = append(rr, fd)
			}
		}
		if len(rr) > 0 {
			input = io.MultiReader(rr...)
		} else {
			input = os.Stdin
		}
		userUrl, err = cmd.bestMirror(input)
		cmd.closeReaders(rr)
		if err != nil {
			return err
		}
	case len(args) == 0:
		return new(flags.Error)
	default:
		userUrl = args[0]
	}

	if _, ok := cmd.options.HeaderMap[hUserAgentKey]; !ok {
		cmd.options.HeaderMap[hUserAgentKey] = userAgents[cmd.options.UserAgent]
	}

	// All users of cookiejar should import "golang.org/x/net/publicsuffix"
	jar, err := cookiejar.New(&cookiejar.Options{PublicSuffixList: publicsuffix.List})
	if err != nil {
		return err
	}

	var session *Session
	err = backoff.Retry(cmd.Ctx, exponential.New(), time.Hour,
		func(count int, _ time.Time) (retry bool, err error) {
			defer func() {
				if retry {
					cmd.logger.Println("Retrying...")
				}
			}()
			session, err = cmd.follow(jar, userUrl)
			if e, ok := err.(*HttpError); ok {
				if isServerError(e.StatusCode) {
					return count+1 != int(cmd.options.MaxRetry), err
				}
			}
			return false, err
		})
	if err != nil {
		return err
	}

	if lastSession == nil {
		lastSession = new(Session)
		err := lastSession.loadState(session.SuggestedFileName + ".json")
		if err != nil {
			lastSession = nil
		}
	}

	if lastSession != nil {
		err := lastSession.checkSums(session)
		if err != nil {
			return err
		}
		err = lastSession.checkPartsSize()
		if err != nil {
			return err
		}
		lastSession.Location = session.Location
		session = lastSession
	} else if cmd.options.Parts > 0 {
		session.HeaderMap = cmd.options.HeaderMap
		session.Parts = session.calcParts(cmd.dlogger, cmd.options.Parts)
		err := session.checkExistingFile(cmd.Out, cmd.options.ForceOverwrite)
		if err != nil {
			return err
		}
	}

	session.writeSummary(cmd.Out, cmd.options.Quiet)

	progress := mpb.NewWithContext(cmd.Ctx,
		mpb.ContainerOptional(mpb.WithOutput(cmd.Out), !cmd.options.Quiet),
		mpb.ContainerOptional(mpb.WithOutput(nil), cmd.options.Quiet),
		mpb.ContainerOptional(mpb.WithDebugOutput(cmd.Err), cmd.options.Debug),
		mpb.WithRefreshRate(refreshRate*time.Millisecond),
		mpb.WithWidth(64),
	)

	var eg errgroup.Group
	transport, err := cmd.getTransport(true)
	if err != nil {
		return err
	}
	if cmd.options.Timeout == 0 {
		cmd.options.Timeout = 15
	}
	single := len(session.Parts) == 1
	for i, p := range session.Parts {
		if p.isDone() {
			continue
		}
		p.order = i
		p.single = single
		p.quiet = cmd.options.Quiet
		p.maxTry = int(cmd.options.MaxRetry)
		p.jar = jar
		p.transport = transport
		p.name = fmt.Sprintf("P%02d", i+1)
		p.dlogger = setupLogger(cmd.Err, fmt.Sprintf("[%s] ", p.name), !cmd.options.Debug)
		req, err := http.NewRequest(http.MethodGet, session.Location, nil)
		if err != nil {
			cmd.logger.Fatalf("%s: %s", p.name, err.Error())
		}
		req.URL.User = cmd.userInfo
		cmd.applyHeaders(req)
		p := p // https://golang.org/doc/faq#closures_and_goroutines
		eg.Go(func() error {
			return p.download(cmd.Ctx, progress, req, cmd.options.Timeout)
		})
	}

	err = eg.Wait()

	session.Parts = filter(session.Parts, func(p *Part) bool { return !p.Skip })

	if err != nil {
		// preserve user provided url
		session.Location = userUrl
		stateName := session.SuggestedFileName + ".json"
		var stateMedia io.Writer
		f, fcErr := os.Create(stateName)
		if fcErr != nil {
			defer cmd.debugOrPrintErr(fcErr, false)
			stateMedia = cmd.Err
		} else {
			defer func() {
				if e := f.Close(); e != nil {
					cmd.debugOrPrintErr(e, false)
				}
			}()
			stateMedia = f
		}
		progress.Wait()
		e := session.dumpState(stateMedia)
		if e != nil {
			cmd.debugOrPrintErr(e, false)
		} else if fcErr == nil {
			fmt.Fprintf(cmd.Err, "session state saved to %q\n", stateName)
		}
		return err
	}

	if cmd.options.Parts > 0 {
		written := session.totalWritten()
		if session.ContentLength > 0 && written != session.ContentLength {
			return errors.Errorf("Corrupted download: ContentLength=%d Saved=%d", session.ContentLength, written)
		}
		err := session.concatenateParts(cmd.dlogger, progress)
		if err != nil {
			return err
		}
		progress.Wait()
		cmd.logger.Printf("%q saved [%d/%d]", session.SuggestedFileName, session.ContentLength, written)
		if cmd.options.JSONFileName != "" {
			return os.Remove(cmd.options.JSONFileName)
		}
	}
	return nil
}

func (cmd Cmd) follow(jar http.CookieJar, userUrl string) (session *Session, err error) {
	if hc, ok := cmd.options.HeaderMap[hCookie]; ok {
		var cookies []*http.Cookie
		for _, cookie := range strings.Split(hc, "; ") {
			pair := strings.SplitN(cookie, "=", 2)
			if len(pair) != 2 {
				continue
			}
			cookies = append(cookies, &http.Cookie{Name: pair[0], Value: pair[1]})
		}
		if u, err := url.Parse(userUrl); err == nil {
			jar.SetCookies(u, cookies)
		}
	}

	var redirected bool
	var client *http.Client
	defer func() {
		if redirected && client != nil {
			client.CloseIdleConnections()
		}
		err = errors.Wrap(err, "follow")
	}()

	transport, err := cmd.getTransport(false)
	if err != nil {
		return nil, err
	}
	client = &http.Client{
		Transport: transport,
		Jar:       jar,
		CheckRedirect: func(_ *http.Request, via []*http.Request) error {
			if len(via) > maxRedirects {
				return errors.WithMessagef(ErrMaxRedirects, "stopped after %d redirects", maxRedirects)
			}
			return http.ErrUseLastResponse
		},
	}

	for {
		cmd.logger.Printf("GET: %s", userUrl)
		req, err := http.NewRequest(http.MethodGet, userUrl, nil)
		if err != nil {
			return nil, err
		}
		req.URL.User = cmd.userInfo
		cmd.applyHeaders(req)

		resp, err := client.Do(req.WithContext(cmd.Ctx))
		if err != nil {
			return nil, err
		}
		cmd.logger.Printf("HTTP response: %s", resp.Status)
		if cookies := jar.Cookies(req.URL); len(cookies) != 0 {
			cmd.dlogger.Println("CookieJar:")
			for _, cookie := range cookies {
				cmd.dlogger.Printf("  %q", cookie)
			}
		}

		if isRedirect(resp.StatusCode) {
			redirected = true
			loc, err := resp.Location()
			if err != nil {
				return nil, err
			}
			userUrl = loc.String()
			// don't bother closing resp.Body here,
			// it will be closed by underlying RoundTripper
			continue
		}

		if resp.StatusCode != http.StatusOK {
			return nil, &HttpError{resp.StatusCode, resp.Status}
		}

		if name := cmd.options.OutFileName; name == "" {
			name = parseContentDisposition(resp.Header.Get(hContentDisposition))
			if name == "" {
				if nURL, err := url.Parse(userUrl); err == nil {
					nURL.RawQuery = ""
					name, err = url.QueryUnescape(nURL.String())
					if err != nil {
						name = nURL.String()
					}
				} else {
					name = userUrl
				}
				name = filepath.Base(name)
			}
			cmd.options.OutFileName = name
		}

		session = &Session{
			Location:          userUrl,
			SuggestedFileName: cmd.options.OutFileName,
			AcceptRanges:      resp.Header.Get("Accept-Ranges"),
			ContentType:       resp.Header.Get("Content-Type"),
			ContentMD5:        resp.Header.Get("Content-MD5"),
			StatusCode:        resp.StatusCode,
			ContentLength:     resp.ContentLength,
		}
		return session, resp.Body.Close()
	}
}

func (cmd Cmd) applyHeaders(req *http.Request) {
	for k, v := range cmd.options.HeaderMap {
		if k == hCookie {
			continue
		}
		req.Header.Set(k, v)
	}
}

func (cmd Cmd) getTransport(pooled bool) (transport *http.Transport, err error) {
	if pooled {
		transport = cleanhttp.DefaultPooledTransport()
	} else {
		transport = cleanhttp.DefaultTransport()
	}
	transport.TLSHandshakeTimeout = time.Duration(cmd.options.Timeout) * time.Second
	if cmd.options.InsecureSkipVerify {
		transport.TLSClientConfig = &tls.Config{InsecureSkipVerify: true}
	} else if cmd.options.CertsFileName != "" {
		caCerts, err := ioutil.ReadFile(cmd.options.CertsFileName)
		if err != nil {
			return nil, err
		}
		caCertPool := x509.NewCertPool()
		caCertPool.AppendCertsFromPEM(caCerts)
		transport.TLSClientConfig = &tls.Config{RootCAs: caCertPool}
	}
	return transport, nil
}

func (cmd Cmd) bestMirror(input io.Reader) (best string, err error) {
	defer func() {
		// just add method name, without stack trace at the point
		err = errors.WithMessage(err, "bestMirror")
	}()
	urls, err := readLines(input)
	if err != nil {
		return
	}

	var readyWg sync.WaitGroup
	start := make(chan struct{})
	first := make(chan string, 1)
	client := cleanhttp.DefaultClient()
	ctx, cancel := context.WithTimeout(cmd.Ctx, 15*time.Second)
	defer cancel()

	for _, u := range urls {
		req, err := http.NewRequest(http.MethodGet, u, nil)
		if err != nil {
			cmd.dlogger.Printf("skipping %q: %s", u, err.Error())
			continue
		}
		readyWg.Add(1)
		req.URL.User = cmd.userInfo
		u := u // https://golang.org/doc/faq#closures_and_goroutines
		subscribe(&readyWg, start, func() {
			cmd.dlogger.Printf("fetching: %q", u)
			resp, err := client.Do(req.WithContext(ctx))
			if err != nil {
				cmd.dlogger.Printf("fetch error: %s", err.Error())
			}
			if resp == nil || resp.Body == nil {
				return
			}
			defer resp.Body.Close()

			if resp.StatusCode != http.StatusOK {
				cmd.dlogger.Printf("fetch %q unexpected status: %s", u, resp.Status)
				return
			}
			select {
			case first <- u:
			default:
				// first has already been found
			}
		})
	}
	readyWg.Wait()
	close(start)
	select {
	case best = <-first:
		cmd.dlogger.Printf("best mirror found: %q", best)
	case <-ctx.Done():
	}
	return best, ctx.Err()
}

func (cmd Cmd) readPassword() (string, error) {
	fmt.Fprint(cmd.Out, "Enter Password: ")
	bytePassword, err := terminal.ReadPassword(int(syscall.Stdin))
	if err != nil {
		return "", err
	}
	fmt.Fprintln(cmd.Out)
	return string(bytePassword), nil
}

func (cmd Cmd) closeReaders(rr []io.Reader) {
	for _, r := range rr {
		if closer, ok := r.(io.Closer); ok {
			if err := closer.Close(); err != nil {
				cmd.dlogger.Printf("close failed: %s", err.Error())
			}
		}
	}
}

func subscribe(wg *sync.WaitGroup, start <-chan struct{}, fn func()) {
	go func() {
		wg.Done()
		<-start
		fn()
	}()
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

func isRedirect(status int) bool {
	return status > 299 && status < 400
}

func isServerError(status int) bool {
	return status > 499 && status < 500
}

func readLines(r io.Reader) ([]string, error) {
	var lines []string
	scanner := bufio.NewScanner(r)
	for scanner.Scan() {
		text := strings.TrimSpace(scanner.Text())
		if len(text) == 0 || strings.HasPrefix(text, "#") {
			continue
		}
		lines = append(lines, text)
	}
	return lines, scanner.Err()
}

func filter(parts []*Part, predicate func(*Part) bool) []*Part {
	filtered := parts[:0]
	for _, p := range parts {
		if predicate(p) {
			filtered = append(filtered, p)
		}
	}
	return filtered
}

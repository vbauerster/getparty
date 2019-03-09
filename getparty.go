package getparty

import (
	"bufio"
	"context"
	"fmt"
	"io"
	"io/ioutil"
	"log"
	"net/http"
	"net/url"
	"os"
	"os/signal"
	"path/filepath"
	"regexp"
	"strings"
	"sync"
	"syscall"
	"time"

	cleanhttp "github.com/hashicorp/go-cleanhttp"
	flags "github.com/jessevdk/go-flags"
	"github.com/pkg/errors"
	"github.com/vbauerster/mpb/v4"
	"golang.org/x/crypto/ssh/terminal"
	"golang.org/x/sync/errgroup"
)

const (
	cmdName     = "getparty"
	projectHome = "https://github.com/vbauerster/getparty"

	maxRedirects        = 10
	hUserAgentKey       = "User-Agent"
	hContentDisposition = "Content-Disposition"
	hRange              = "Range"
)

// https://regex101.com/r/N4AovD/3
var reContentDisposition = regexp.MustCompile(`filename[^;\n=]*=(['"](.*?)['"]|[^;\n]*)`)

var userAgents = map[string]string{
	"chrome":  "Mozilla/5.0 (Macintosh; Intel Mac OS X 10_13_4) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/65.0.3325.181 Safari/537.36",
	"firefox": "Mozilla/5.0 (Macintosh; Intel Mac OS X 10.13; rv:59.0) Gecko/20100101 Firefox/59.0",
	"safari":  "Mozilla/5.0 (Macintosh; Intel Mac OS X 10_13_4) AppleWebKit/605.1.15 (KHTML, like Gecko) Version/11.1 Safari/605.1.15",
}

type ExpectedError struct {
	Err error
}

func (e ExpectedError) Error() string {
	return e.Err.Error()
}

// Options struct, represents cmd line options
type Options struct {
	Parts        uint              `short:"p" long:"parts" value-name:"n" default:"2" description:"number of parts"`
	Timeout      uint              `short:"t" long:"timeout" value-name:"sec" default:"15" description:"context timeout"`
	OutFileName  string            `short:"o" long:"output" value-name:"filename" description:"user defined output"`
	JSONFileName string            `short:"c" long:"continue" value-name:"state.json" description:"resume download from the last session"`
	UserAgent    string            `short:"a" long:"user-agent" choice:"chrome" choice:"firefox" choice:"safari" default:"chrome" description:"User-Agent header"`
	BestMirror   bool              `short:"b" long:"best-mirror" description:"pickup the fastest mirror"`
	Quiet        bool              `short:"q" long:"quiet" description:"quiet mode, no progress bars"`
	AuthUser     string            `short:"u" long:"username" description:"basic http auth username"`
	AuthPass     string            `long:"password" description:"basic http auth password"`
	HeaderMap    map[string]string `long:"header" value-name:"key:value" description:"arbitrary http header"`
	Debug        bool              `long:"debug" description:"enable debug to stderr"`
	Version      bool              `long:"version" description:"show version"`
}

type Cmd struct {
	Out, Err io.Writer
	userInfo *url.Userinfo
	options  *Options
	parser   *flags.Parser
	logger   *log.Logger
	dlogger  *log.Logger
}

func (cmd Cmd) Exit(err error) int {
	if err == nil {
		return 0
	}
	switch e := errors.Cause(err).(type) {
	case *flags.Error:
		if e.Type == flags.ErrHelp {
			return 0
		}
		cmd.parser.WriteHelp(cmd.Err)
		return 2
	case ExpectedError:
		if cmd.options.Debug {
			cmd.dlogger.Printf("exit error: %+v", err)
		} else {
			fmt.Fprintf(cmd.Err, "exit error: %v\n", err)
		}
		return 1
	default:
		if cmd.options.Debug {
			cmd.dlogger.Printf("unexpected error: %+v", err)
		} else {
			fmt.Fprintf(cmd.Err, "unexpected error: %v\n", err)
		}
		return 3
	}
}

func (cmd *Cmd) Run(args []string, version string) (err error) {
	defer func() {
		// just add method name, without stack trace at the point
		err = errors.WithMessage(err, "run")
	}()
	cmd.options = new(Options)
	cmd.parser = flags.NewParser(cmd.options, flags.Default)
	cmd.parser.Name = cmdName
	cmd.parser.Usage = "[OPTIONS] url"

	args, err = cmd.parser.ParseArgs(args)
	if err != nil {
		return err
	}

	if cmd.options.Version {
		fmt.Fprintf(cmd.Out, "%s: %s\n", cmdName, version)
		fmt.Fprintf(cmd.Out, "Project home: %s\n", projectHome)
		return nil
	}

	if len(args) == 0 && cmd.options.JSONFileName == "" && !cmd.options.BestMirror {
		return new(flags.Error)
	}

	if cmd.options.Quiet {
		cmd.Out = ioutil.Discard
	}
	cmd.logger = log.New(cmd.Out, "", log.LstdFlags)
	cmd.dlogger = log.New(ioutil.Discard, fmt.Sprintf("[%s] ", cmdName), log.LstdFlags)
	if cmd.options.Debug {
		cmd.dlogger.SetOutput(cmd.Err)
	}

	ctx, cancel := backgroundContext()
	defer cancel()

	if cmd.options.AuthUser != "" {
		if cmd.options.AuthPass == "" {
			cmd.options.AuthPass, err = cmd.readPassword()
			if err != nil {
				return err
			}
		}
		cmd.userInfo = url.UserPassword(cmd.options.AuthUser, cmd.options.AuthPass)
	}

	var userUrl string
	var lastSession *Session

	switch {
	case cmd.options.JSONFileName != "":
		lastSession = new(Session)
		if err := lastSession.loadState(cmd.options.JSONFileName); err != nil {
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
		userUrl, err = cmd.bestMirror(ctx, input)
		cmd.closeReaders(rr)
		if err != nil {
			return err
		}
	default:
		userUrl = args[0]
	}

	if _, ok := cmd.options.HeaderMap[hUserAgentKey]; !ok {
		cmd.options.HeaderMap[hUserAgentKey] = userAgents[cmd.options.UserAgent]
	}
	session, err := cmd.follow(ctx, userUrl)
	if err != nil {
		if ctx.Err() == context.Canceled {
			// most probably user hit ^C, so mark as expected
			return ExpectedError{ctx.Err()}
		}
		return err
	}

	if lastSession != nil {
		if lastSession.ContentMD5 != session.ContentMD5 {
			return errors.Errorf(
				"ContentMD5 mismatch: remote %q expected %q",
				session.ContentMD5, lastSession.ContentMD5,
			)
		}
		if lastSession.ContentLength != session.ContentLength {
			return errors.Errorf(
				"ContentLength mismatch: remote %d expected %d",
				session.ContentLength, lastSession.ContentLength,
			)
		}
		lastSession.Location = session.Location
		session = lastSession
	} else if cmd.options.Parts > 0 {
		session.HeaderMap = cmd.options.HeaderMap
		session.Parts = session.calcParts(int64(cmd.options.Parts))
		if _, err := os.Stat(session.SuggestedFileName); err == nil {
			var answer string
			fmt.Printf("File %q already exists, overwrite? [y/n] ", session.SuggestedFileName)
			if _, err := fmt.Scanf("%s", &answer); err != nil {
				return err
			}
			switch strings.ToLower(answer) {
			case "y", "yes":
				if err := session.removeFiles(); err != nil {
					return err
				}
			default:
				return nil
			}
		}
	}

	session.writeSummary(cmd.Out)
	progress := mpb.NewWithContext(ctx,
		mpb.WithOutput(cmd.Out),
		mpb.ContainerOptOnCond(mpb.WithDebugOutput(cmd.Err), func() bool {
			return cmd.options.Debug
		}),
		mpb.ContainerOptOnCond(mpb.WithManualRefresh(make(chan time.Time)), func() bool {
			return cmd.options.Quiet
		}),
		mpb.WithRefreshRate(180*time.Millisecond),
		mpb.WithWidth(60),
	)

	var eg errgroup.Group
	transport := cleanhttp.DefaultPooledTransport()
	tlsTimeout := uint64(transport.TLSHandshakeTimeout)
	for i, p := range session.Parts {
		if p.isDone() {
			continue
		}
		p.order = i
		p.transport = transport
		p.tlsTimeout = tlsTimeout
		p.name = fmt.Sprintf("p#%02d", i+1)
		p.dlogger = log.New(ioutil.Discard, fmt.Sprintf("[%s] ", p.name), log.LstdFlags)
		if cmd.options.Debug {
			p.dlogger.SetOutput(cmd.Err)
		}
		req, err := http.NewRequest(http.MethodGet, session.Location, nil)
		if err != nil {
			cmd.logger.Fatalf("%s: %v", p.name, err)
		}
		req.URL.User = cmd.userInfo
		for k, v := range cmd.options.HeaderMap {
			req.Header.Set(k, v)
		}
		p := p // https://golang.org/doc/faq#closures_and_goroutines
		eg.Go(func() error {
			return p.download(ctx, progress, req, cmd.options.Timeout)
		})
	}

	err = eg.Wait()
	session.actualPartsOnly()

	if err != nil {
		if ctx.Err() == context.Canceled {
			// most probably user hit ^C, so mark as expected
			err = ExpectedError{ctx.Err()}
		} else {
			cancel()
		}
	} else if cmd.options.Parts > 0 {
		if written := session.totalWritten(); written == session.ContentLength || session.ContentLength <= 0 {
			err = session.concatenateParts(cmd.dlogger, progress)
			progress.Wait()
			if err != nil {
				return err
			}
			fmt.Fprintln(cmd.Out)
			cmd.logger.Printf("%q saved [%d/%d]", session.SuggestedFileName, session.ContentLength, written)
			if cmd.options.JSONFileName != "" {
				return os.Remove(cmd.options.JSONFileName)
			}
			return nil
		}
	}

	progress.Wait()

	// preserve user provided url
	session.Location = userUrl
	stateName := session.SuggestedFileName + ".json"
	if e := session.saveState(stateName); e == nil {
		fmt.Fprintln(cmd.Out)
		cmd.logger.Printf("session state saved to %q", stateName)
	} else if err == nil {
		err = e
	}
	return err
}

func (cmd Cmd) follow(ctx context.Context, userUrl string) (session *Session, err error) {
	defer func() {
		if session == nil && err == nil {
			err = ExpectedError{
				errors.Errorf("maximum number of redirects (%d) followed", maxRedirects),
			}
		}
		// just add method name, without stack trace at the point
		err = errors.WithMessage(err, "follow")
	}()
	client := cleanhttp.DefaultClient()
	client.CheckRedirect = func(*http.Request, []*http.Request) error {
		return http.ErrUseLastResponse
	}
	next := userUrl
	for i := 0; i < maxRedirects; i++ {
		cmd.logger.Printf("GET: %s", next)
		req, err := http.NewRequest(http.MethodGet, next, nil)
		if err != nil {
			return nil, err
		}
		req.URL.User = cmd.userInfo
		for k, v := range cmd.options.HeaderMap {
			req.Header.Set(k, v)
		}

		resp, err := client.Do(req.WithContext(ctx))
		if err != nil {
			return nil, err
		}
		cmd.logger.Printf("HTTP response: %s", resp.Status)

		if isRedirect(resp.StatusCode) {
			loc, err := resp.Location()
			if err != nil {
				return nil, err
			}
			next = loc.String()
			// don't bother closing resp.Body here,
			// it will be closed by underlying RoundTripper
			continue
		}

		if resp.StatusCode != http.StatusOK {
			return nil, errors.Errorf("unexpected status: %s", resp.Status)
		}

		if name := cmd.options.OutFileName; name == "" {
			name = parseContentDisposition(resp.Header.Get(hContentDisposition))
			if name == "" {
				if nURL, err := url.Parse(next); err == nil {
					nURL.RawQuery = ""
					name, err = url.QueryUnescape(nURL.String())
					if err != nil {
						name = nURL.String()
					}
				} else {
					name = next
				}
				name = filepath.Base(name)
			}
			cmd.options.OutFileName = name
		}

		session = &Session{
			Location:          next,
			SuggestedFileName: cmd.options.OutFileName,
			AcceptRanges:      resp.Header.Get("Accept-Ranges"),
			ContentType:       resp.Header.Get("Content-Type"),
			StatusCode:        resp.StatusCode,
			ContentLength:     resp.ContentLength,
			ContentMD5:        resp.Header.Get("Content-MD5"),
		}
		return session, resp.Body.Close()
	}
	return
}

func (cmd Cmd) bestMirror(ctx context.Context, input io.Reader) (best string, err error) {
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
	ctx, cancel := context.WithTimeout(ctx, 15*time.Second)
	defer cancel()

	for _, u := range urls {
		req, err := http.NewRequest(http.MethodGet, u, nil)
		if err != nil {
			cmd.dlogger.Printf("skipping %q: %v", u, err)
			continue
		}
		readyWg.Add(1)
		req.URL.User = cmd.userInfo
		u := u // https://golang.org/doc/faq#closures_and_goroutines
		subscribe(&readyWg, start, func() {
			cmd.dlogger.Printf("fetching: %q", u)
			resp, err := client.Do(req.WithContext(ctx))
			if err != nil {
				cmd.dlogger.Printf("fetch error: %v", err)
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
				cmd.dlogger.Printf("close failed: %v", err)
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

func readLines(r io.Reader) ([]string, error) {
	var lines []string
	scanner := bufio.NewScanner(r)
	for scanner.Scan() {
		lines = append(lines, scanner.Text())
	}
	return lines, scanner.Err()
}

func backgroundContext() (context.Context, func()) {
	ctx, cancel := context.WithCancel(context.Background())
	quit := make(chan os.Signal, 1)
	signal.Notify(quit, syscall.SIGINT, syscall.SIGTERM)

	go func() {
		defer signal.Stop(quit)
		<-quit
		cancel()
	}()

	return ctx, cancel
}

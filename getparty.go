package getparty

import (
	"bufio"
	"context"
	"encoding/json"
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
	"runtime"
	"strings"
	"syscall"
	"time"

	"github.com/jessevdk/go-flags"
	"github.com/pkg/errors"
	"github.com/vbauerster/mpb"
	"golang.org/x/crypto/ssh/terminal"
	"golang.org/x/sync/errgroup"
)

const (
	maxRedirects = 10
	cmdName      = "getparty"
	projectHome  = "https://github.com/vbauerster/getparty"
)

// https://regex101.com/r/N4AovD/3
var contentDispositionRe = regexp.MustCompile(`filename[^;\n=]*=(['"](.*?)['"]|[^;\n]*)`)

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
	Parts        uint   `short:"p" long:"parts" value-name:"n" default:"2" description:"number of parts"`
	OutFileName  string `short:"o" long:"output" value-name:"filename" description:"user defined output"`
	JSONFileName string `short:"c" long:"continue" value-name:"state" description:"resume download from the last saved state file"`
	UserAgent    string `short:"a" long:"user-agent" choice:"chrome" choice:"firefox" choice:"safari" default:"chrome" description:"User-Agent header"`
	BestMirror   bool   `short:"b" long:"best-mirror" description:"pickup the fastest mirror, will read from stdin"`
	AuthUser     string `short:"u" long:"username" description:"basic http auth username"`
	AuthPass     string `long:"password" description:"basic http auth password"`
	Debug        bool   `long:"debug" description:"enable debug to stderr"`
	Version      bool   `long:"version" description:"show version"`
}

type Cmd struct {
	Out, Err  io.Writer
	userAgent string
	userInfo  *url.Userinfo
	options   *Options
	parser    *flags.Parser
	logger    *log.Logger
	dlogger   *log.Logger
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
		if !cmd.options.Debug {
			fmt.Fprintf(cmd.Err, "exit error: %v\n", err)
		}
		cmd.dlogger.Printf("exit error: %+v\n", err)
		return 1
	default:
		if !cmd.options.Debug {
			fmt.Fprintf(cmd.Err, "unexpected error: %v\n", err)
		}
		cmd.dlogger.Printf("unexpected error: %+v\n", err)
		return 3
	}
}

func (cmd *Cmd) Run(args []string, version string) (err error) {
	defer func() {
		// just add method name, without stack trace at the point
		err = errors.WithMessage(err, "Run")
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
		fmt.Fprintf(cmd.Out, "%s: %s (runtime: %s)\n", cmdName, version, runtime.Version())
		fmt.Fprintf(cmd.Out, "Project home: %s\n", projectHome)
		return nil
	}

	if len(args) == 0 && cmd.options.JSONFileName == "" && !cmd.options.BestMirror {
		return new(flags.Error)
	}

	cmd.logger = log.New(cmd.Out, "", log.LstdFlags)
	cmd.dlogger = log.New(ioutil.Discard, fmt.Sprintf("[%s] ", cmdName), log.LstdFlags)
	if cmd.options.Debug {
		cmd.dlogger.SetOutput(cmd.Err)
	}

	ctx, cancel := context.WithCancel(context.Background())
	cmd.quitHandler(cancel)
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

	cmd.userAgent = userAgents[cmd.options.UserAgent]

	if cmd.options.BestMirror {
		args[0], err = cmd.bestMirror(ctx)
		if err != nil {
			return err
		}
	}

	var al *ActualLocation
	var userUrl string // url before redirect

	if cmd.options.JSONFileName != "" {
		al, err = cmd.loadActualLocation(cmd.options.JSONFileName)
		if err != nil {
			return err
		}
		userUrl = al.Location
		remote, err := cmd.follow(ctx, userUrl, al.SuggestedFileName)
		if err != nil {
			return err
		}
		if al.ContentLength != remote.ContentLength {
			return errors.Errorf("ContentLength mismatch: remote length %d, expected length %d", remote.ContentLength, al.ContentLength)
		}
		al.Location = remote.Location
	} else {
		userUrl = args[0]
		al, err = cmd.follow(ctx, userUrl, cmd.options.OutFileName)
		if err != nil {
			return err
		}
		// ports are appending, so ask to overwrite
		if _, err := os.Stat(al.SuggestedFileName); err == nil {
			var answer string
			fmt.Printf("File %q already exists, overwrite? [y/n] ", al.SuggestedFileName)
			_, err = fmt.Scanf("%s", &answer)
			if err != nil {
				return err
			}
			switch strings.ToLower(answer) {
			case "y", "yes":
				err = os.Remove(al.SuggestedFileName)
				if err != nil {
					return err
				}
			default:
				return nil
			}
		}
		al.Parts = al.calcParts(int64(cmd.options.Parts))
	}

	al.writeSummary(cmd.Out)

	eg, ctx := errgroup.WithContext(ctx)
	pb := mpb.New(
		mpb.WithOutput(cmd.Out),
		mpb.WithWidth(60),
		mpb.WithFormat("[=>-|"),
		mpb.WithContext(ctx),
	)

	for i, p := range al.Parts {
		if p.Skip {
			continue
		}
		p := p
		i := i
		eg.Go(func() error {
			logger := log.New(ioutil.Discard, fmt.Sprintf("[p#%02d] ", i+1), log.LstdFlags)
			if cmd.options.Debug {
				logger.SetOutput(cmd.Err)
			}
			return p.download(ctx, pb, logger, cmd.userInfo, cmd.userAgent, al.Location, i)
		})
	}

	err = eg.Wait()
	if err != nil {
		// if it's user context.Canceled error, mark as expected error
		if ctx.Err() == context.Canceled {
			err = ExpectedError{err}
		}
	} else if al.totalWritten() == al.ContentLength {
		err = al.concatenateParts(cmd.dlogger)
		pb.Wait()
		if err != nil {
			return err
		}
		fmt.Fprintln(cmd.Out)
		cmd.logger.Printf("%q saved [%[2]d/%[2]d]\n", al.SuggestedFileName, al.ContentLength)
		if cmd.options.JSONFileName != "" {
			return os.Remove(cmd.options.JSONFileName)
		}
		return nil
	} else {
		// unexpected
		err = errors.Errorf("ContentLength mismatch: remote length %d, written %d", al.ContentLength, al.totalWritten())
	}

	// marshal with original url
	name, _ := al.marshalState(userUrl)
	pb.Wait()
	fmt.Fprintln(cmd.Out)
	cmd.logger.Printf("state saved to %q\n", name)
	return err
}

func (cmd Cmd) quitHandler(cancel context.CancelFunc) {
	quit := make(chan os.Signal, 1)
	signal.Notify(quit, syscall.SIGINT, syscall.SIGTERM)

	go func() {
		defer signal.Stop(quit)
		<-quit
		cancel()
	}()
}

func (cmd Cmd) loadActualLocation(filename string) (*ActualLocation, error) {
	fd, err := os.Open(filename)
	if err != nil {
		return nil, err
	}
	defer func() {
		if e := fd.Close(); e != nil {
			cmd.dlogger.Printf("close %q: %v", fd.Name(), e)
		}
	}()

	decoder := json.NewDecoder(fd)
	al := new(ActualLocation)
	err = decoder.Decode(al)
	return al, err
}

func (cmd Cmd) follow(ctx context.Context, userUrl, outFileName string) (al *ActualLocation, err error) {
	defer func() {
		// just add method name, without stack trace at the point
		err = errors.WithMessage(err, "follow")
	}()
	client := &http.Client{
		CheckRedirect: func(*http.Request, []*http.Request) error {
			return http.ErrUseLastResponse
		},
	}
	next := userUrl
	var redirectCount int
	for {
		cmd.logger.Printf("GET: %s\n", next)
		req, err := http.NewRequest(http.MethodGet, next, nil)
		if err != nil {
			return nil, err
		}
		req.Close = true
		req.URL.User = cmd.userInfo
		req.Header.Set("User-Agent", cmd.userAgent)

		resp, err := client.Do(req.WithContext(ctx))
		if err != nil {
			return nil, err
		}
		cmd.logger.Println("HTTP response:", resp.Status)

		if isRedirect(resp.StatusCode) {
			loc, err := resp.Location()
			if err != nil {
				return nil, err
			}
			redirectCount++
			if redirectCount > maxRedirects {
				return nil, ExpectedError{
					errors.Errorf("maximum number of redirects (%d) followed", maxRedirects),
				}
			}
			next = loc.String()
			// don't bother closing resp.Body here,
			// it will be closed by underlying RoundTripper
			continue
		}

		if resp.StatusCode != http.StatusOK {
			return nil, ExpectedError{
				errors.Errorf("unprocessable http status %q", resp.Status),
			}
		}

		if outFileName == "" {
			outFileName = parseContentDisposition(resp.Header.Get("Content-Disposition"))
		}
		if outFileName == "" {
			var path string
			if nURL, err := url.Parse(next); err == nil {
				nURL.RawQuery = ""
				path, err = url.QueryUnescape(nURL.String())
				if err != nil {
					path = nURL.String()
				}
			} else {
				path = next
			}
			outFileName = filepath.Base(path)
		}

		al = &ActualLocation{
			Location:          next,
			SuggestedFileName: outFileName,
			AcceptRanges:      resp.Header.Get("Accept-Ranges"),
			ContentType:       resp.Header.Get("Content-Type"),
			StatusCode:        resp.StatusCode,
			ContentLength:     resp.ContentLength,
			ContentMD5:        resp.Header.Get("Content-MD5"),
		}

		if err := resp.Body.Close(); err != nil {
			cmd.dlogger.Printf("%s resp.Body.Close() failed: %v\n", next, err)
		}
		return al, nil
	}
}

func (cmd Cmd) bestMirror(ctx context.Context) (fastest string, err error) {
	defer func() {
		// just add method name, without stack trace at the point
		err = errors.WithMessage(err, "bestMirror")
	}()
	lines, err := readLines(os.Stdin)
	if err != nil {
		return fastest, err
	}
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()
	ch := make(chan string, len(lines))
	for _, url := range lines {
		go cmd.fetch(ctx, url, ch)
	}
	select {
	case fastest = <-ch:
	case <-time.After(3 * time.Second):
		err = ExpectedError{errors.New("timeout")}
	}
	return fastest, err
}

func (cmd Cmd) fetch(ctx context.Context, rawUrl string, first chan<- string) {
	req, err := http.NewRequest(http.MethodHead, rawUrl, nil)
	if err != nil {
		cmd.dlogger.Println("fetch:", err)
		return
	}
	req.Close = true
	req.URL.User = cmd.userInfo
	resp, err := http.DefaultClient.Do(req.WithContext(ctx))
	if err != nil {
		cmd.dlogger.Println("fetch:", err)
		return
	}
	defer func() {
		if resp.Body != nil {
			if err := resp.Body.Close(); err != nil {
				cmd.dlogger.Printf("%s resp.Body.Close() failed: %v\n", rawUrl, err)
			}
		}
	}()

	if resp.StatusCode != http.StatusOK {
		cmd.dlogger.Printf("%s %q\n", resp.Status, rawUrl)
		return
	}
	first <- rawUrl
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

func parseContentDisposition(input string) string {
	groups := contentDispositionRe.FindAllStringSubmatch(input, -1)
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

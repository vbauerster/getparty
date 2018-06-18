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

type Error struct {
	Err error
}

func (e Error) Error() string {
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
	logger    *log.Logger
	dlogger   *log.Logger
}

func (s *Cmd) Run(args []string, version string) (exitHandler func() int) {
	options := new(Options)
	parser := flags.NewParser(options, flags.Default)
	parser.Name = cmdName
	parser.Usage = "[OPTIONS] url"

	var err error
	exitHandler = func() int {
		if err == nil {
			return 0
		}
		switch e := errors.Cause(err).(type) {
		case *flags.Error:
			if e.Type == flags.ErrHelp {
				return 0
			}
			parser.WriteHelp(s.Err)
			return 2
		case Error:
			if options.Debug {
				s.dlogger.Printf("exit error: %+v\n", err)
			} else {
				fmt.Fprintf(s.Err, "exit error: %v\n", err)
			}
			return 1
		default:
			if options.Debug {
				s.dlogger.Printf("unexpected error: %+v\n", err)
			} else {
				fmt.Fprintf(s.Err, "unexpected error: %v\n", err)
			}
			return 3
		}
	}

	args, err = parser.ParseArgs(args)
	if err != nil {
		return
	}

	if options.Version {
		fmt.Fprintf(s.Out, "%s: %s (runtime: %s)\n", cmdName, version, runtime.Version())
		fmt.Fprintf(s.Out, "Project home: %s\n", projectHome)
		return
	}

	if len(args) == 0 && options.JSONFileName == "" && !options.BestMirror {
		err = new(flags.Error)
		return
	}

	s.logger = log.New(s.Out, "", log.LstdFlags)
	s.dlogger = log.New(ioutil.Discard, fmt.Sprintf("[%s] ", cmdName), log.LstdFlags)
	if options.Debug {
		s.dlogger.SetOutput(s.Err)
	}

	ctx, cancel := context.WithCancel(context.Background())
	s.quitHandler(cancel)

	var al *ActualLocation
	var userUrl string // url before redirect

	if options.AuthUser != "" {
		if options.AuthPass == "" {
			options.AuthPass, err = s.readPassword()
			if err != nil {
				err = errors.WithMessage(errors.Wrap(err, "unable to read password"), "run")
				return
			}
		}
		s.userInfo = url.UserPassword(options.AuthUser, options.AuthPass)
	}

	s.userAgent = userAgents[options.UserAgent]

	if options.BestMirror {
		args, err = s.bestMirror(ctx)
		if err != nil {
			err = errors.WithMessage(err, "run")
			return
		}
	}

	if options.JSONFileName != "" {
		al, err = s.loadActualLocation(options.JSONFileName)
		if err != nil {
			err = errors.WithMessage(errors.Wrap(Error{err}, "load state failed"), "run")
			return
		}
		userUrl = al.Location
		temp, e := s.follow(ctx, userUrl, al.SuggestedFileName)
		if e != nil {
			err = e
			return
		}
		if al.ContentLength != temp.ContentLength {
			err = errors.Wrap(Error{
				errors.Errorf("ContentLength mismatch: expected %d, got %d", al.ContentLength, temp.ContentLength),
			}, "run")
			return
		}
		al.Location = temp.Location
	} else {
		userUrl = args[0]
		al, err = s.follow(ctx, userUrl, options.OutFileName)
		if err != nil {
			return
		}
		al.Parts = al.calcParts(int64(options.Parts))
	}

	al.writeSummary(s.Out)

	eg, ctx := errgroup.WithContext(ctx)
	pb := mpb.New(
		mpb.WithOutput(s.Out),
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
			if options.Debug {
				logger.SetOutput(s.Err)
			}
			return p.download(ctx, pb, logger, s.userInfo, s.userAgent, al.Location, i)
		})
	}

	err = eg.Wait()
	if ctx.Err() != nil && err != nil {
		err = errors.Wrap(Error{err}, "run")
	}
	pb.Wait()

	fmt.Fprintln(s.Out)
	if al.totalWritten() == al.ContentLength {
		if e := al.concatenateParts(s.dlogger); err == nil {
			err = e
		}
		state := al.Parts[0].FileName + ".json"
		if e := os.Remove(state); err == nil {
			if !os.IsNotExist(e) {
				err = e
			}
		}
		s.logger.Printf("%q saved [%[2]d/%[2]d]\n", al.SuggestedFileName, al.ContentLength)
	} else if al.ContentLength > 0 && al.StatusCode < 300 {
		// marshal with original url
		name, e := al.marshalState(userUrl)
		if err == nil {
			err = e
		}
		s.logger.Printf("state saved to %q\n", name)
	}

	return
}

func (s *Cmd) quitHandler(cancel context.CancelFunc) {
	quit := make(chan os.Signal, 1)
	signal.Notify(quit, syscall.SIGINT, syscall.SIGTERM)

	go func() {
		defer signal.Stop(quit)
		<-quit
		cancel()
	}()
}

func (s *Cmd) loadActualLocation(filename string) (*ActualLocation, error) {
	fd, err := os.Open(filename)
	if err != nil {
		return nil, err
	}
	defer func() {
		if e := fd.Close(); e != nil {
			s.dlogger.Printf("close %q: %v", fd.Name(), e)
		}
	}()

	decoder := json.NewDecoder(fd)
	al := new(ActualLocation)
	err = decoder.Decode(al)
	return al, err
}

func (s *Cmd) follow(ctx context.Context, userUrl, outFileName string) (*ActualLocation, error) {
	client := &http.Client{
		CheckRedirect: func(*http.Request, []*http.Request) error {
			return http.ErrUseLastResponse
		},
	}
	next := userUrl
	var redirectCount int
	for {
		s.logger.Printf("GET: %s\n", next)
		req, err := http.NewRequest(http.MethodGet, next, nil)
		if err != nil {
			return nil, err
		}
		req.Close = true
		req.URL.User = s.userInfo
		req.Header.Set("User-Agent", s.userAgent)

		resp, err := client.Do(req.WithContext(ctx))
		if err != nil {
			return nil, err
		}
		s.logger.Println("HTTP response:", resp.Status)

		if isRedirect(resp.StatusCode) {
			loc, err := resp.Location()
			if err != nil {
				return nil, err
			}
			redirectCount++
			if redirectCount > maxRedirects {
				return nil, errors.Wrap(Error{
					errors.Errorf("maximum number of redirects (%d) followed", maxRedirects),
				}, "follow")
			}
			next = loc.String()
			// don't bother closing resp.Body here,
			// it will be closed by underlying RoundTripper
			continue
		}

		if resp.StatusCode != http.StatusOK {
			return nil, errors.Wrap(Error{
				errors.Errorf("unprocessable http status %q", resp.Status),
			}, "follow")
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

		al := &ActualLocation{
			Location:          next,
			SuggestedFileName: outFileName,
			AcceptRanges:      resp.Header.Get("Accept-Ranges"),
			ContentType:       resp.Header.Get("Content-Type"),
			StatusCode:        resp.StatusCode,
			ContentLength:     resp.ContentLength,
			ContentMD5:        resp.Header.Get("Content-MD5"),
		}

		if err := resp.Body.Close(); err != nil {
			s.dlogger.Printf("%s resp.Body.Close() failed: %v\n", next, err)
		}
		return al, nil
	}
}

func (s *Cmd) bestMirror(ctx context.Context) ([]string, error) {
	lines, err := readLines(os.Stdin)
	if err != nil {
		return nil, errors.WithMessage(
			errors.Wrap(Error{err}, "unable to read from stdin"),
			"best-mirror",
		)
	}
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()
	first := make(chan string, len(lines))
	for _, url := range lines {
		go s.fetch(ctx, url, first)
	}
	select {
	case u := <-first:
		return []string{u}, nil
	case <-time.After(3 * time.Second):
		return nil, errors.Wrap(Error{errors.New("timeout")}, "best-mirror")
	}
}

func (s *Cmd) fetch(ctx context.Context, rawUrl string, first chan<- string) {
	req, err := http.NewRequest(http.MethodHead, rawUrl, nil)
	if err != nil {
		s.dlogger.Println("fetch:", err)
		return
	}
	req.Close = true
	req.URL.User = s.userInfo
	resp, err := http.DefaultClient.Do(req.WithContext(ctx))
	if err != nil {
		s.dlogger.Println("fetch:", err)
		return
	}
	defer func() {
		if resp.Body != nil {
			if err := resp.Body.Close(); err != nil {
				s.dlogger.Printf("%s resp.Body.Close() failed: %v\n", rawUrl, err)
			}
		}
	}()

	if resp.StatusCode != http.StatusOK {
		s.dlogger.Printf("%s %q\n", resp.Status, rawUrl)
		return
	}
	first <- rawUrl
}

func (s *Cmd) readPassword() (string, error) {
	fmt.Fprint(s.Out, "Enter Password: ")
	bytePassword, err := terminal.ReadPassword(int(syscall.Stdin))
	if err != nil {
		return "", err
	}
	fmt.Fprintln(s.Out)
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

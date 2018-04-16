package gp

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"io"
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
	"golang.org/x/sync/errgroup"
)

const (
	maxRedirects = 10
	cmdName      = "getparty"
	userAgent    = "Mozilla/5.0 (Macintosh; Intel Mac OS X 10_12_2) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/55.0.2883.95 Safari/537.36"
	projectHome  = "https://github.com/vbauerster/getparty"
)

// https://regex101.com/r/N4AovD/3
var contentDispositionRe = regexp.MustCompile(`filename[^;\n=]*=(['"](.*?)['"]|[^;\n]*)`)

type Error struct {
	Err error
}

func (e Error) Error() string {
	return e.Err.Error()
}

// Options struct, represents cmd line options
type Options struct {
	Parts        uint   `short:"p" long:"parts" default:"2" description:"number of parts"`
	OutFileName  string `short:"o" long:"output-file" value-name:"NAME" description:"force output file name"`
	JSONFileName string `short:"c" long:"continue" value-name:"JSON" description:"resume download from the last saved json file"`
	BestMirror   bool   `short:"b" long:"best-mirror" description:"pickup the fastest mirror. Will read from stdin"`
	Version      bool   `long:"version" description:"show version"`
}

type Cmd struct {
	Out, Err  io.Writer
	logger    *log.Logger
	errLogger *log.Logger
}

func (s *Cmd) Run(args []string, version string) (help func(), err error) {
	options := new(Options)
	parser := flags.NewParser(options, flags.Default)
	parser.Name = cmdName
	parser.Usage = "[OPTIONS] url"

	help = func() { parser.WriteHelp(s.Err) }

	args, err = parser.ParseArgs(args)
	if err != nil {
		return help, err
	}

	if options.Version {
		fmt.Fprintf(s.Out, "%s: %s (runtime: %s)\n", cmdName, version, runtime.Version())
		fmt.Fprintf(s.Out, "Project home: %s\n", projectHome)
		return help, nil
	}

	if len(args) == 0 && options.JSONFileName == "" && !options.BestMirror {
		return help, new(flags.Error)
	}

	s.logger = log.New(s.Out, "", log.LstdFlags)
	s.errLogger = log.New(s.Err, "[ERR] ", log.LstdFlags)

	ctx, cancel := context.WithCancel(context.Background())
	s.quitHandler(cancel)

	if options.BestMirror {
		lines, err := readLines(os.Stdin)
		if err != nil {
			return help, errors.WithMessage(errors.Wrap(Error{err}, "unable read from stdin"), "best-mirror")
		}
		mctx, mcancel := context.WithCancel(ctx)
		first := make(chan string, len(lines))
		for _, u := range lines {
			go fetch(mctx, s.errLogger, u, first)
		}
		select {
		case murl := <-first:
			mcancel()
			args = []string{murl}
		case <-time.After(3 * time.Second):
			mcancel()
			return help, errors.Wrap(Error{errors.New("timeout")}, "best-mirror")
		}
	}

	var al *ActualLocation
	var userURL string

	if options.JSONFileName != "" {
		al, err = s.loadActualLocation(options.JSONFileName)
		if err != nil {
			return help, errors.Wrap(Error{err}, "load state failed")
		}
		userURL = al.Location
		temp, err := s.follow(ctx, userURL, userAgent, al.SuggestedFileName)
		if err != nil {
			return help, err
		}
		if al.ContentLength != temp.ContentLength {
			return help, errors.Wrap(Error{
				errors.Errorf("ContentLength mismatch: expected %d, got %d", al.ContentLength, temp.ContentLength),
			}, "Run")
		}
		al.Location = temp.Location
	} else {
		userURL = args[0]
		al, err = s.follow(ctx, userURL, userAgent, options.OutFileName)
		if err != nil {
			return help, err
		}
		al.calcParts(int(options.Parts))
	}

	al.writeSummary(s.Out)

	eg, ctx := errgroup.WithContext(ctx)
	pb := mpb.New(mpb.Output(s.Out), mpb.WithWidth(64), mpb.WithContext(ctx))
	for i, p := range al.Parts {
		if p.Skip {
			s.errLogger.Printf("skip: %#v", p)
			continue
		}
		p := p
		i := i
		eg.Go(func() error {
			return p.download(ctx, s.errLogger, pb, al.Location, i)
		})
	}

	err = eg.Wait()
	al.deleteUnnecessaryParts()
	pb.Wait()

	fmt.Fprintln(s.Out)
	if al.totalWritten() == al.ContentLength {
		if e := al.concatenateParts(s.errLogger); err == nil {
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
		name, e := al.marshalState(userURL)
		if err == nil {
			err = e
		}
		s.logger.Printf("state saved to %q\n", name)
	}

	if ctx.Err() != nil && err != nil {
		err = errors.Wrap(Error{err}, "Run")
	}
	return help, err
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
			s.errLogger.Printf("close file %q: %v", filename, e)
		}
	}()

	decoder := json.NewDecoder(fd)
	al := new(ActualLocation)
	err = decoder.Decode(al)
	return al, err
}

func (s *Cmd) follow(ctx context.Context, userURL, userAgent, outFileName string) (*ActualLocation, error) {
	client := &http.Client{
		CheckRedirect: func(*http.Request, []*http.Request) error {
			return http.ErrUseLastResponse
		},
	}
	next := userURL
	var redirectsFollowed int
	for {
		s.logger.Printf("GET: %s\n", next)
		req, err := http.NewRequest(http.MethodGet, next, nil)
		if err != nil {
			return nil, err
		}
		req = req.WithContext(ctx)
		req.Close = true
		req.Header.Set("User-Agent", userAgent)

		resp, err := client.Do(req)
		if err != nil {
			return nil, err
		}
		s.logger.Println("HTTP response:", resp.Status)

		if isRedirect(resp.StatusCode) {
			loc, err := resp.Location()
			if err != nil {
				return nil, err
			}
			redirectsFollowed++
			if redirectsFollowed > maxRedirects {
				return nil, errors.Wrap(Error{
					errors.Errorf("maximum number of redirects (%d) followed", maxRedirects),
				}, "follow")
			}
			next = loc.String()
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
			if path, err := url.QueryUnescape(next); err == nil {
				outFileName = filepath.Base(path)
			} else {
				outFileName = filepath.Base(next)
			}
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
			s.errLogger.Printf("close resp body failed: %v\n", err)
		}
		return al, nil
	}
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

func fetch(ctx context.Context, errLogger *log.Logger, url string, first chan<- string) {
	req, err := http.NewRequest(http.MethodHead, url, nil)
	if err != nil {
		errLogger.Println(err)
		return
	}
	req.Close = true
	resp, err := http.DefaultClient.Do(req.WithContext(ctx))
	if err != nil {
		errLogger.Println(err)
		return
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		errLogger.Printf("%s %q\n", resp.Status, url)
		return
	}
	first <- url
}

func readLines(r io.Reader) ([]string, error) {
	if closer, ok := r.(io.Closer); ok {
		defer closer.Close()
	}
	var lines []string
	scanner := bufio.NewScanner(r)
	for scanner.Scan() {
		lines = append(lines, scanner.Text())
	}
	return lines, scanner.Err()
}

// getparty
// Copyright (C) 2016-2017 Vladimir Bauer
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

package main

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
	"sync"
	"syscall"
	"time"

	"github.com/jessevdk/go-flags"
	"github.com/vbauerster/mpb"
	"github.com/vbauerster/mpb/decor"
)

const (
	rr           = 120 * time.Millisecond
	maxRedirects = 10
	cmdName      = "getparty"
	userAgent    = "Mozilla/5.0 (Macintosh; Intel Mac OS X 10_12_2) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/55.0.2883.95 Safari/537.36"
	projectHome  = "https://github.com/vbauerster/getparty"
)

var (
	version              = "devel"
	contentDispositionRe *regexp.Regexp
	// Command line options
	options Options
	// flags parser
	parser *flags.Parser

	bLogger *log.Logger
)

// ActualLocation represents server's status 200 or 206 response meta data
// It never holds redirect responses
type ActualLocation struct {
	Location          string
	SuggestedFileName string
	ContentMD5        string
	AcceptRanges      string
	StatusCode        int
	ContentLength     int64
	Parts             []*Part
}

// Part represents state of each download part
type Part struct {
	Name                 string
	Start, Stop, Written int64
	Skip                 bool
	fail                 bool
}

// Options struct, represents cmd line options
type Options struct {
	Parts        uint   `short:"p" long:"parts" default:"2" description:"number of parts"`
	Timeout      uint   `short:"t" long:"timeout" description:"download timeout in seconds"`
	OutFileName  string `short:"o" long:"output-file" value-name:"NAME" description:"force output file name"`
	JSONFileName string `short:"c" long:"continue" value-name:"JSON" description:"resume download from the last saved json file"`
	Mirrors      bool   `short:"m" long:"best-mirror" description:"pickup the fastest mirror. Will read from stdin"`
	Version      bool   `long:"version" description:"show version"`
}

func init() {
	// https://regex101.com/r/N4AovD/3
	contentDispositionRe = regexp.MustCompile(`filename[^;\n=]*=(['"](.*?)['"]|[^;\n]*)`)
	parser = flags.NewParser(&options, flags.Default)
	parser.Name = cmdName
	parser.Usage = "[OPTIONS] url"
	bLogger = log.New(os.Stdout, "[ ", log.LstdFlags)
}

func main() {
	args, err := parser.Parse()
	if err != nil {
		if e, ok := err.(*flags.Error); ok && e.Type == flags.ErrHelp {
			os.Exit(0)
		} else {
			fmt.Println()
			parser.WriteHelp(os.Stderr)
			os.Exit(1)
		}
	}

	if options.Version {
		fmt.Printf("%s %s (runtime: %s)\n", cmdName, version, runtime.Version())
		fmt.Printf("Project home: %s\n", projectHome)
		os.Exit(0)
	}

	var wg sync.WaitGroup
	recoverIfPanic := func(id int) {
		if e := recover(); e != nil {
			log.Printf("Part %d: %+v\n", id, e)
		}
		wg.Done()
	}
	var al *ActualLocation
	var userURL string
	var cancel context.CancelFunc
	ctx := context.Background()

	if options.Mirrors {
		lines, err := readLines(os.Stdin)
		exitOnError(err)
		mctx, mcancel := context.WithCancel(ctx)
		first := make(chan string, len(lines))
		for _, url := range lines {
			go fetch(mctx, url, first)
		}
		args = []string{<-first}
		mcancel()
	}

	if options.Timeout > 0 {
		ctx, cancel = context.WithTimeout(ctx, time.Duration(options.Timeout)*time.Second)
	} else {
		ctx, cancel = context.WithCancel(ctx)
	}

	pb := mpb.New(mpb.WithWidth(64),
		mpb.WithRefreshRate(rr),
		mpb.WithContext(ctx),
		mpb.WithBeforeRenderFunc(sortByBarNameFunc()))

	if len(args) > 0 {
		url, err := parseURL(args[0])
		exitOnError(err)
		userURL = url.String()
		al, err = follow(userURL, userAgent, options.OutFileName, 0)
		exitOnError(err)
		if al.StatusCode == http.StatusOK {
			al.calcParts(int(options.Parts))
			for i, p := range al.Parts {
				wg.Add(1)
				go func(n int, part *Part) {
					defer recoverIfPanic(n)
					part.download(ctx, pb, al.Location, n)
				}(i, p)
			}
		}
	} else if options.JSONFileName != "" {
		al, err = loadActualLocationFromJSON(options.JSONFileName)
		exitOnError(err)
		userURL = al.Location
		temp, err := follow(userURL, userAgent, al.SuggestedFileName, al.totalWritten())
		exitOnError(err)
		if al.ContentLength != temp.ContentLength {
			log.Fatalf("ContentLength mismatch: expected %d, got %d", al.ContentLength, temp.ContentLength)
		}
		al.Location = temp.Location
		for i, p := range al.Parts {
			if !p.Skip {
				wg.Add(1)
				go func(n int, part *Part) {
					defer recoverIfPanic(n)
					part.download(ctx, pb, al.Location, n)
				}(i, p)
			}
		}
	} else {
		fmt.Println("Nothing to do...")
		parser.WriteHelp(os.Stderr)
		os.Exit(1)
	}

	go onCancelSignal(cancel)

	wg.Wait()
	for _, part := range al.Parts {
		if part.fail {
			time.Sleep(rr * time.Millisecond)
			cancel()
			break
		}
	}
	pb.Stop()

	al.deleteUnnecessaryParts()
	if al.totalWritten() == al.ContentLength {
		logIfError(al.concatenateParts())
		json := al.Parts[0].Name + ".json"
		if _, err := os.Stat(json); err == nil { // if exists
			logIfError(os.Remove(json))
		}
		fmt.Println()
		bLogf("%q saved [%[2]d/%[2]d]\n", al.SuggestedFileName, al.ContentLength)
	} else if al.ContentLength > 0 && al.StatusCode != http.StatusNotFound {
		logIfError(al.marshalState(userURL))
	}
}

func (p *Part) download(ctx context.Context, pb *mpb.Progress, url string, n int) {
	if p.Stop-p.Start == p.Written-1 {
		return
	}

	req, err := http.NewRequest(http.MethodGet, url, nil)
	if err != nil {
		panic(err)
	}
	req = req.WithContext(ctx)
	req.Header.Set("User-Agent", userAgent)
	req.Header.Set("Range", p.getRange())

	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		panic(err)
	}

	total := p.Stop - p.Start + 1
	if resp.StatusCode == http.StatusOK {
		if n > 0 {
			p.Skip = true
			return
		}
		total = resp.ContentLength
		p.Stop = total - 1
	} else if resp.StatusCode != http.StatusPartialContent {
		return
	}

	messageCh := make(chan string, 1)
	failureCh := make(chan struct{})

	fail := func(err error) {
		close(failureCh)
		p.fail = true
		errs := strings.Split(err.Error(), ":")
		messageCh <- errs[len(errs)-1]
	}

	padding := 18
	bar := pb.AddBar(total, mpb.BarID(n),
		mpb.PrependDecorators(
			decor.StaticName(fmt.Sprintf("p#%02d:", n+1), 0, 0),
			countersDecorator(messageCh, padding),
		),
		mpb.AppendDecorators(speedDecorator(failureCh)),
	)

	var dst *os.File
	if p.Written > 0 && resp.StatusCode != http.StatusOK {
		dst, err = os.OpenFile(p.Name, os.O_APPEND|os.O_WRONLY, 0644)
		bar.ResumeFill('+', p.Written)
		bar.Incr(int(p.Written))
	} else {
		dst, err = os.Create(p.Name)
		p.Written = 0
	}
	if err != nil {
		fail(err)
		return
	}
	defer dst.Close()

	for i := 0; i <= 3; i++ {
		if i > 0 {
			time.Sleep(2 * time.Second)
			messageCh <- fmt.Sprintf("Retrying (%d)", i)
			req.Header.Set("Range", p.getRange())
			resp, err = http.DefaultClient.Do(req)
			if err != nil {
				if resp != nil {
					resp.Body.Close()
				}
				if i == 3 {
					fail(err)
				}
				continue
			}
		}

		err = p.writeToFile(dst, resp, bar)

		if err == nil || ctx.Err() != nil {
			if total <= 0 {
				bar.Complete()
			}
			return
		}

		if i == 3 {
			fail(err)
		} else {
			messageCh <- "Error..."
		}
	}
}

func (p *Part) writeToFile(dst *os.File, resp *http.Response, bar *mpb.Bar) (err error) {
	defer resp.Body.Close()

	reader := bar.ProxyReader(resp.Body)

	for i := 0; i < 3; i++ {
		var written int64
		written, err = io.Copy(dst, reader)
		p.Written += written
		if err != nil && isTemporary(err) {
			time.Sleep(1e9)
			continue
		}
		break
	}

	return
}

func (p *Part) getRange() string {
	if p.Stop <= 0 {
		return ""
	}
	start := p.Start
	if p.Written > 0 {
		start = start + p.Written
	}
	return fmt.Sprintf("bytes=%d-%d", start, p.Stop)
}

func (al *ActualLocation) calcParts(parts int) {
	if parts == 0 {
		return
	}
	partSize := al.ContentLength / int64(parts)
	if partSize <= 0 {
		parts = 1
	}
	al.Parts = make([]*Part, parts)

	stop := al.ContentLength
	start := stop
	for i := parts - 1; i > 0; i-- {
		stop = start - 1
		start = stop - partSize
		al.Parts[i] = &Part{
			Name:  fmt.Sprintf("%s.part%d", al.SuggestedFileName, i),
			Start: start,
			Stop:  stop,
		}
	}
	stop = start - 1
	if stop < 0 {
		stop = 0
	}
	al.Parts[0] = &Part{
		Name: al.SuggestedFileName,
		Stop: stop,
	}
}

func (al *ActualLocation) concatenateParts() error {
	fpart0, err := os.OpenFile(al.Parts[0].Name, os.O_APPEND|os.O_WRONLY, 0644)
	if err != nil {
		return err
	}

	buf := make([]byte, 2*1024)
	for i := 1; i < len(al.Parts); i++ {
		fparti, err := os.Open(al.Parts[i].Name)
		if err != nil {
			return err
		}
		for {
			n, err := fparti.Read(buf[:])
			_, errw := fpart0.Write(buf[:n])
			if errw != nil {
				return err
			}
			if err != nil {
				if err == io.EOF {
					break
				}
				return err
			}
		}
		logIfError(fparti.Close())
		logIfError(os.Remove(al.Parts[i].Name))
	}
	return fpart0.Close()
}

func (al *ActualLocation) deleteUnnecessaryParts() {
	for i := len(al.Parts) - 1; i >= 0; i-- {
		if al.Parts[i].Skip {
			al.Parts = append(al.Parts[:i], al.Parts[i+1:]...)
		}
	}
}

func (al *ActualLocation) marshalState(userURL string) error {
	state := al.SuggestedFileName + ".json"
	log.Printf("writing state to %q\n", state)
	al.Location = userURL // preserve user provided url
	data, err := json.Marshal(al)
	if err != nil {
		return err
	}
	dst, err := os.Create(state)
	if err != nil {
		return err
	}
	_, err = dst.Write(data)
	if errc := dst.Close(); err == nil {
		err = errc
	}
	return err
}

func (al *ActualLocation) totalWritten() int64 {
	var total int64
	for _, p := range al.Parts {
		total += p.Written
	}
	return total
}

func follow(userURL, userAgent, outFileName string, totalWritten int64) (*ActualLocation, error) {
	client := &http.Client{
		CheckRedirect: func(_ *http.Request, _ []*http.Request) error {
			return http.ErrUseLastResponse
		},
		Transport: &http.Transport{DisableKeepAlives: true},
	}
	next := userURL
	var al *ActualLocation
	var redirectsFollowed int
	for {
		bLogf("%s\n", next)
		fmt.Printf("HTTP request sent, awaiting response... ")
		req, err := http.NewRequest(http.MethodGet, next, nil)
		if err != nil {
			fmt.Println()
			return nil, err
		}
		req.Header.Set("User-Agent", userAgent)

		resp, err := client.Do(req)
		// if redirection failure, both resp and err are non-nil
		if resp != nil {
			defer resp.Body.Close()
		}

		if err != nil {
			fmt.Println()
			return nil, err
		}
		fmt.Println(resp.Status)

		if outFileName == "" {
			outFileName = parseContentDisposition(resp.Header.Get("Content-Disposition"))
			if outFileName == "" {
				if path, err := url.QueryUnescape(userURL); err == nil {
					outFileName = filepath.Base(path)
				} else {
					outFileName = filepath.Base(userURL)
				}
			}
		}

		al = &ActualLocation{
			Location:          next,
			SuggestedFileName: outFileName,
			AcceptRanges:      resp.Header.Get("Accept-Ranges"),
			StatusCode:        resp.StatusCode,
			ContentLength:     resp.ContentLength,
			ContentMD5:        resp.Header.Get("Content-MD5"),
		}

		if !isRedirect(resp.StatusCode) {
			if resp.StatusCode == http.StatusOK {
				humanSize := decor.Format(resp.ContentLength).To(decor.Unit_KiB)
				format := fmt.Sprintf("Length: %%s [%s]\n", resp.Header.Get("Content-Type"))
				var length string
				if totalWritten > 0 && al.AcceptRanges != "" {
					remaining := resp.ContentLength - totalWritten
					length = fmt.Sprintf("%d (%s), %d (%s) remaining",
						resp.ContentLength,
						humanSize,
						remaining,
						decor.Format(remaining).To(decor.Unit_KiB))
				} else if resp.ContentLength < 0 {
					length = "unknown"
				} else {
					length = fmt.Sprintf("%d (%s)", resp.ContentLength, humanSize)
				}
				fmt.Printf(format, length)
				if al.AcceptRanges == "" {
					fmt.Println("Looks like server doesn't support ranges (no party, no resume)")
				}
				fmt.Printf("Saving to: %q\n\n", outFileName)
			}
			break
		}

		loc, err := resp.Location()
		if err != nil {
			return nil, err
		}
		redirectsFollowed++
		if redirectsFollowed > maxRedirects {
			return nil, fmt.Errorf("maximum number of redirects (%d) followed", maxRedirects)
		}
		next = loc.String()
		fmt.Printf("Location: %s [following]\n", next)
	}
	return al, nil
}

func onCancelSignal(cancel context.CancelFunc) {
	defer cancel()
	sigs := make(chan os.Signal, 2)
	signal.Notify(sigs, syscall.SIGINT, syscall.SIGTERM)
	sig := <-sigs
	fmt.Println()
	log.Printf("%v: canceling...\n", sig)
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

func loadActualLocationFromJSON(filename string) (*ActualLocation, error) {
	file, err := os.Open(filename)
	if err != nil {
		return nil, err
	}

	decoder := json.NewDecoder(file)
	al := new(ActualLocation)
	err = decoder.Decode(al)
	if errc := file.Close(); err == nil {
		err = errc
	}
	return al, err
}

func parseURL(uri string) (*url.URL, error) {
	if !strings.Contains(uri, "://") && !strings.HasPrefix(uri, "//") {
		uri = "//" + uri
	}

	url, err := url.Parse(uri)
	if err != nil {
		return nil, err
	}

	if url.Scheme == "" {
		url.Scheme = "http"
		if !strings.HasSuffix(url.Host, ":80") {
			url.Scheme += "s"
		}
	}
	return url, nil
}

func isRedirect(status int) bool {
	return status > 299 && status < 400
}

func exitOnError(err error) {
	if err != nil {
		log.Fatal(err)
	}
}

func logIfError(err error) {
	if err != nil {
		log.Println(err)
	}
}

func fetch(ctx context.Context, url string, first chan<- string) {
	req, err := http.NewRequest(http.MethodGet, url, nil)
	if err != nil {
		return
	}
	resp, err := http.DefaultClient.Do(req.WithContext(ctx))
	if err != nil {
		return
	}
	defer resp.Body.Close()
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

func bLogf(format string, a ...interface{}) {
	bLogger.Printf("] "+format, a...)
}

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
	"time"

	"github.com/jessevdk/go-flags"
	"github.com/vbauerster/mpb"
)

const (
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
	Parts             map[int]*Part
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
	JsonFileName string `short:"c" long:"continue" value-name:"state.json" description:"resume download from last saved json state"`
	Parts        int    `short:"p" long:"parts" default:"2" description:"number of parts"`
	Timeout      int    `short:"t" long:"timeout" description:"download timeout in seconds"`
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
			log.Printf("Unexpected panic in part %d: %+v\n", id, e)
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
		first := make(chan string, len(args))
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

	pb := mpb.New(ctx).SetWidth(60).BeforeRenderFunc(sortByBarNameFunc())

	if len(args) > 0 {
		userURL = parseURL(args[0]).String()
		al, err = follow(userURL, userAgent, 0)
		exitOnError(err)
		if al.StatusCode == http.StatusOK {
			if al.ContentLength <= 0 {
				options.Parts = 1
			}
			al.calcParts(options.Parts)
			for n, part := range al.Parts {
				wg.Add(1)
				go func(n int, part *Part) {
					defer recoverIfPanic(n)
					part.download(ctx, pb, al.Location, n)
				}(n, part)
			}
		}
	} else if options.JsonFileName != "" {
		al, err = loadActualLocationFromJSON(options.JsonFileName)
		exitOnError(err)
		userURL = al.Location
		temp, err := follow(userURL, userAgent, al.totalWritten())
		exitOnError(err)
		al.Location = temp.Location
		for n, part := range al.Parts {
			if !part.Skip {
				wg.Add(1)
				go func(n int, part *Part) {
					defer recoverIfPanic(n)
					part.download(ctx, pb, al.Location, n)
				}(n, part)
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
			time.Sleep(100 * time.Millisecond)
			cancel()
			break
		}
	}
	pb.Stop()

	if al.totalWritten() == al.ContentLength {
		logIfError(al.concatenateParts())
		json := al.Parts[1].Name + ".json"
		if _, err := os.Stat(json); err == nil {
			logIfError(os.Remove(json))
		}
		fmt.Println()
		bLogf("%q saved [%[2]d/%[2]d]\n", al.SuggestedFileName, al.ContentLength)
	} else if al.ContentLength > 0 {
		al.deleteUnnecessaryParts()
		logIfError(al.marshalState(userURL))
	}
}

func (p *Part) getRange() string {
	if p.Stop < 0 {
		return "bytes=0-"
	}
	start := p.Start
	if p.Written > 0 {
		start = start + p.Written
	}
	return fmt.Sprintf("bytes=%d-%d", start, p.Stop)
}

func (p *Part) download(ctx context.Context, pb *mpb.Progress, url string, n int) {
	if p.Stop-p.Start == p.Written-1 {
		return
	}

	name := fmt.Sprintf("part#%02d:", n)
	req, err := http.NewRequest(http.MethodGet, url, nil)
	if err != nil {
		log.Println(name, err)
		return
	}
	req = req.WithContext(ctx)
	req.Header.Set("User-Agent", userAgent)
	req.Header.Set("Range", p.getRange())

	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		log.Println(name, err)
		return
	}

	total := p.Stop - p.Start + 1
	if resp.StatusCode == http.StatusOK {
		if n > 1 {
			p.Skip = true
			return
		}
		total = resp.ContentLength
		p.Stop = total - 1
	} else if resp.StatusCode != http.StatusPartialContent {
		log.Printf("%s status %d\n", name, resp.StatusCode)
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
	bar := pb.AddBarWithID(total, n).
		PrependName(name, 0).
		PrependFunc(countersDecorator(messageCh, padding))

	if total > 0 {
		bar.AppendFunc(etaDecorator(failureCh))
	}

	var dst *os.File
	if p.Written > 0 && resp.StatusCode != http.StatusOK {
		dst, err = os.OpenFile(p.Name, os.O_APPEND|os.O_WRONLY, 0644)
		bar.IncrWithReFill(int(p.Written), '+')
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
			messageCh <- formRetryMsg(i)
			req.Header.Set("Range", p.getRange())
			resp, err = http.DefaultClient.Do(req)
			if err != nil {
				if i == 3 {
					fail(err)
				}
				continue
			}
		}

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

		logIfError(resp.Body.Close())

		if err == nil || ctx.Err() != nil {
			if total <= 0 {
				bar.Completed()
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

func (al *ActualLocation) calcParts(totalParts int) {
	if totalParts <= 0 {
		totalParts = 1
	}
	partSize := al.ContentLength / int64(totalParts)
	if partSize == 0 {
		partSize = al.ContentLength / 2
	}
	al.Parts = make(map[int]*Part)

	stop := al.ContentLength
	start := stop
	for i := totalParts; i > 1; i-- {
		stop = start - 1
		start = stop - partSize
		al.Parts[i] = &Part{
			Name:  fmt.Sprintf("%s.part%d", al.SuggestedFileName, i),
			Start: start,
			Stop:  stop,
		}
	}
	al.Parts[1] = &Part{
		Name: al.SuggestedFileName,
		Stop: start - 1,
	}
}

func (al *ActualLocation) concatenateParts() error {
	fpart1, err := os.OpenFile(al.Parts[1].Name, os.O_APPEND|os.O_WRONLY, 0644)
	if err != nil {
		return err
	}

	buf := make([]byte, 2*1024)
	for i := 2; i <= len(al.Parts); i++ {
		if al.Parts[i].Skip {
			continue
		}
		fparti, err := os.Open(al.Parts[i].Name)
		if err != nil {
			return err
		}
		for {
			n, err := fparti.Read(buf[:])
			_, errw := fpart1.Write(buf[:n])
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
	return fpart1.Close()
}

func (al *ActualLocation) deleteUnnecessaryParts() {
	for k, v := range al.Parts {
		if v.Skip {
			delete(al.Parts, k)
		}
	}
}

func (al *ActualLocation) marshalState(userURL string) error {
	jsonFileName := al.SuggestedFileName + ".json"
	log.Printf("writing state to %q\n", jsonFileName)
	al.Location = userURL // preserve user provided url
	data, err := json.Marshal(al)
	if err != nil {
		return err
	}
	dst, err := os.Create(jsonFileName)
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

func follow(userURL, userAgent string, totalWritten int64) (*ActualLocation, error) {
	client := &http.Client{
		CheckRedirect: func(_ *http.Request, _ []*http.Request) error {
			return http.ErrUseLastResponse
		},
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
		if err != nil {
			fmt.Println()
			return nil, err
		}
		defer resp.Body.Close()
		fmt.Println(resp.Status)

		suggestedFileName := trimFileName(parseContentDisposition(resp.Header.Get("Content-Disposition")))
		if suggestedFileName == "" {
			suggestedFileName = trimFileName(filepath.Base(userURL))
			suggestedFileName, _ = url.QueryUnescape(suggestedFileName)
		}

		al = &ActualLocation{
			Location:          next,
			SuggestedFileName: suggestedFileName,
			AcceptRanges:      resp.Header.Get("Accept-Ranges"),
			StatusCode:        resp.StatusCode,
			ContentLength:     resp.ContentLength,
			ContentMD5:        resp.Header.Get("Content-MD5"),
		}

		if !isRedirect(resp.StatusCode) {
			if resp.StatusCode == http.StatusOK {
				humanSize := mpb.Format(resp.ContentLength).To(mpb.UnitBytes)
				format := fmt.Sprintf("Length: %%s [%s]\n", resp.Header.Get("Content-Type"))
				var length string
				if totalWritten > 0 && al.AcceptRanges != "" {
					remaining := resp.ContentLength - totalWritten
					length = fmt.Sprintf("%d (%s), %d (%s) remaining",
						resp.ContentLength,
						humanSize,
						remaining,
						mpb.Format(remaining).To(mpb.UnitBytes))
				} else if resp.ContentLength < 0 {
					length = "unknown"
				} else {
					length = fmt.Sprintf("%d (%s)", resp.ContentLength, humanSize)
				}
				fmt.Printf(format, length)
				if al.AcceptRanges == "" {
					fmt.Println("Looks like server doesn't support ranges (no party, no resume)")
				}
				fmt.Printf("Saving to: %q\n\n", suggestedFileName)
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
	sigs := make(chan os.Signal)
	signal.Notify(sigs, os.Interrupt, os.Kill)
	sig := <-sigs
	fmt.Println()
	log.Printf("%v: canceling...\n", sig)
}

func parseContentDisposition(input string) string {
	groups := contentDispositionRe.FindAllStringSubmatch(input, -1)
	if groups == nil {
		return ""
	}
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

func parseURL(uri string) *url.URL {
	if !strings.Contains(uri, "://") && !strings.HasPrefix(uri, "//") {
		uri = "//" + uri
	}

	url, err := url.Parse(uri)
	if err != nil {
		log.Fatalf("could not parse url %q: %v", uri, err)
	}

	if url.Scheme == "" {
		url.Scheme = "http"
		if !strings.HasSuffix(url.Host, ":80") {
			url.Scheme += "s"
		}
	}
	return url
}

func trimFileName(name string) string {
	name = strings.Split(name, "?")[0]
	name = strings.Trim(name, " ")
	return name
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

func formRetryMsg(n int) string {
	var nth string
	switch n {
	case 1:
		nth = "st"
	case 2:
		nth = "nd"
	case 3:
		nth = "rd"
	default:
		nth = "th"
	}
	return fmt.Sprintf("%d%s retry...", n, nth)
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

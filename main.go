// url := "https://homebrew.bintray.com/bottles/youtube-dl-2016.12.12.sierra.bottle.tar.gz"
// url := "https://homebrew.bintray.com/bottles/libtiff-4.0.7.sierra.bottle.tar.gz"
// url := "http://127.0.0.1:8080/libtiff-4.0.7.sierra.bottle.tar.gz"
// url := "http://127.0.0.1:8080/orig.txt"
// url := "https://swtch.com/~rsc/thread/squint.pdf"
package main

import (
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
	"strconv"
	"strings"
	"sync"
	"syscall"

	"github.com/jessevdk/go-flags"
	"github.com/pkg/errors"
	"github.com/vbauerster/mpb"
)

const (
	maxRedirects = 10
	cmdName      = "getparty"
	userAgent    = "Mozilla/5.0 (Macintosh; Intel Mac OS X 10_12_2) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/55.0.2883.95 Safari/537.36"
)

var (
	version              = "devel"
	contentDispositionRe *regexp.Regexp
	// Command line options
	options Options
	// flags parser
	parser *flags.Parser
)

type ActualLocation struct {
	Location          string
	SuggestedFileName string
	ContentMD5        string
	AcceptRanges      string
	StatusCode        int
	ContentLength     int64
	Parts             map[int]*Part
}

type Part struct {
	Name                 string
	Start, Stop, Written int64
	Skip, Only           bool
}

type Options struct {
	State string `short:"c" long:"continue" description:"resume download from last saved json state" value-name:"state.json"`
	Parts int    `short:"p" long:"parts" default:"2" description:"number of parts"`
}

func init() {
	// https://regex101.com/r/N4AovD/3
	contentDispositionRe = regexp.MustCompile(`filename[^;\n=]*=(['"](.*?)['"]|[^;\n]*)`)
	parser = flags.NewParser(&options, flags.Default)
	parser.Name = cmdName
	parser.Usage = "[OPTIONS] url"
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

	var wg sync.WaitGroup
	var al *ActualLocation
	var userURL string
	ctx, cancel := context.WithCancel(context.Background())
	pb := mpb.New(ctx).SetWidth(60)

	if options.State != "" {
		al, err = loadActualLocationFromJson(options.State)
		exitOnError(errors.Wrapf(err, "loading %q", options.State))
		fmt.Printf("al = %+v\n", al)
		for n, part := range al.Parts {
			if !part.Skip {
				fmt.Printf("%d = %+v\n", n, part)
				wg.Add(1)
				go part.download(ctx, &wg, pb, al.Location, userAgent, n)
			}
		}
	} else if len(args) == 1 {
		userURL = parseURL(args[0]).String()
		al, err = follow(userURL, userAgent)
		exitOnError(err)
		if al.AcceptRanges == "bytes" && al.StatusCode == http.StatusOK {
			if al.SuggestedFileName == "" {
				al.SuggestedFileName = filepath.Base(userURL)
			}
			al.getParty(ctx, &wg, pb, options.Parts)
		}
	} else {
		fmt.Println("Nothing to do...")
		parser.WriteHelp(os.Stderr)
		os.Exit(1)
	}

	go marshalOnCancel(cancel, &wg, al, userURL)
	wg.Wait()
	pb.Stop()

	if options.Parts > 1 {
		concatenateParts(al, options.Parts)
	}

	var totalWritten int64
	for _, p := range al.Parts {
		totalWritten += p.Written
	}
	if totalWritten == al.ContentLength {
		exitOnError(renamePart0(al.Parts[0].Name))
	}

	// 	data, err := json.MarshalIndent(al, "", "	")
	// 	if err != nil {
	// 		log.Fatalf("JSON marshaling failed: %s", err)
	// 	}
	// 	fmt.Printf("%s\n", data)

}

func (al *ActualLocation) getParty(ctx context.Context, wg *sync.WaitGroup, pb *mpb.Progress, totalParts int) {
	partSize := al.ContentLength / int64(totalParts)
	if partSize < 0 {
		partSize = al.ContentLength / 2
	}
	al.Parts = make(map[int]*Part)
	for i := 0; i < totalParts; i++ {
		offset := int64(i) * partSize
		start, stop := offset, offset+partSize-1
		name := fmt.Sprintf("%s.part%d", al.SuggestedFileName, i)
		// name := fmt.Sprintf("%s.part%d", "test.tar.gz", i)
		part := &Part{
			Name:  name,
			Start: start,
			Stop:  stop,
		}
		al.Parts[i] = part
		wg.Add(1)
		go part.download(ctx, wg, pb, al.Location, userAgent, i)
	}
}

func (p *Part) download(ctx context.Context, wg *sync.WaitGroup, pb *mpb.Progress, url, userAgent string, n int) {
	defer wg.Done()
	fmt.Printf("%d url = %+v\n", n, url)
	name := fmt.Sprintf("part#%d:", n)
	req, err := http.NewRequest(http.MethodGet, url, nil)
	if err != nil {
		log.Println(name, err)
		return
	}

	start := p.Start
	if p.Written > 0 {
		start = start + p.Written + 1
	}

	req.Header.Set("User-Agent", userAgent)
	rangeStr := fmt.Sprintf("bytes=%d-", start)
	if !p.Only {
		// rangeStr = fmt.Sprintf(rangeStr+"%d", p.Stop)
		rangeStr += strconv.FormatInt(p.Stop, 10)
	}
	req.Header.Set("Range", rangeStr)

	fmt.Fprintf(os.Stderr, "%s Range = %+v\n", name, req.Header.Get("Range"))

	resp, err := http.DefaultClient.Do(req.WithContext(ctx))
	if err != nil {
		log.Printf("%s %v\n", name, err)
		return
	}
	defer resp.Body.Close()

	fmt.Fprintf(os.Stderr, "resp.StatusCode = %+v\n", resp.StatusCode)

	total := p.Stop - p.Start + 1
	if resp.StatusCode == http.StatusOK {
		total = resp.ContentLength
		if n == 0 {
			p.Stop = total - 1
			p.Only = true
		} else {
			p.Skip = true
			return
		}
	} else if resp.StatusCode != 206 {
		fmt.Fprintf(os.Stderr, "status: %d", resp.StatusCode)
		return
	}

	var dst *os.File
	if p.Written > 0 {
		dst, err = os.OpenFile(p.Name, os.O_APPEND|os.O_WRONLY, 0644)
	} else {
		dst, err = os.Create(p.Name)
	}

	if err != nil {
		log.Println(name, err)
		return
	}

	bar := pb.AddBar(total).
		PrependName(name, 0).
		PrependCounters(mpb.UnitBytes, 20).
		AppendETA(-6)
	bar.Incr(int(p.Written))

	// create proxy reader
	reader := bar.ProxyReader(resp.Body)
	// and copy from reader
	p.Written, err = io.Copy(dst, reader)

	if errc := dst.Close(); err == nil {
		err = errc
	}
	if err != nil {
		log.Println(name, err)
	}
}

func follow(userURL string, userAgent string) (*ActualLocation, error) {
	client := &http.Client{
		CheckRedirect: func(_ *http.Request, _ []*http.Request) error {
			return http.ErrUseLastResponse
		},
	}
	next := userURL
	var al *ActualLocation
	var redirectsFollowed int
	for {
		resp, err := getResp(client, next, userAgent)
		if err != nil {
			return nil, err
		}

		al = &ActualLocation{
			Location:          next,
			SuggestedFileName: parseContentDisposition(resp.Header.Get("Content-Disposition")),
			AcceptRanges:      resp.Header.Get("Accept-Ranges"),
			StatusCode:        resp.StatusCode,
			ContentLength:     resp.ContentLength,
			ContentMD5:        resp.Header.Get("Content-MD5"),
		}

		if !isRedirect(resp.StatusCode) {
			break
		}

		loc, err := resp.Location()
		if err != nil {
			return nil, errors.Wrap(err, "unable to follow redirect")
		}
		redirectsFollowed++
		if redirectsFollowed > maxRedirects {
			return nil, errors.Errorf("maximum number of redirects (%d) followed", maxRedirects)
		}
		next = loc.String()
	}
	return al, nil
}

func getResp(client *http.Client, url, userAgent string) (*http.Response, error) {
	req, err := http.NewRequest(http.MethodGet, url, nil)
	if err != nil {
		return nil, errors.Wrapf(err, "cannot make request with %q", url)
	}
	req.Header.Set("User-Agent", userAgent)
	// req.Header.Set("Range", "bytes=0-")
	resp, err := client.Do(req)
	if err != nil {
		return nil, errors.Wrapf(err, "cannot get %q", url)
	}
	defer resp.Body.Close()
	return resp, nil
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

func renamePart0(path string) error {
	ext := filepath.Ext(path)
	if ext == ".part0" {
		return os.Rename(path, path[0:len(path)-len(ext)])
	}
	return errors.Errorf("expected *.part0, got: %s", path)
}

func concatenateParts(al *ActualLocation, totalParts int) {
	openErrMsg := "cannot open %q"
	writeErrMsg := "cannot write to %q"
	readErrMsg := "cannot read from %q"
	part0 := al.Parts[0].Name
	if _, err := os.Stat(al.Parts[1].Name); err == nil {
		fpart0, err := os.OpenFile(part0, os.O_APPEND|os.O_WRONLY, 0644)
		exitOnError(errors.Wrapf(err, openErrMsg, part0))

		buf := make([]byte, 2048)
		for i := 1; i < totalParts; i++ {
			parti := al.Parts[i].Name
			fparti, err := os.Open(parti)
			exitOnError(errors.Wrapf(err, openErrMsg, parti))
			for {
				n, err := fparti.Read(buf[0:])
				_, errw := fpart0.Write(buf[0:n])
				exitOnError(errors.Wrapf(errw, writeErrMsg, part0))
				if err != nil {
					if err == io.EOF {
						break
					}
					exitOnError(errors.Wrapf(err, readErrMsg, parti))
				}
			}
			logIfError(fparti.Close())
			logIfError(os.Remove(parti))
		}
		logIfError(fpart0.Close())
	}
}

func marshalOnCancel(cancel context.CancelFunc, wg *sync.WaitGroup, al *ActualLocation, userURL string) {
	sigs := make(chan os.Signal)
	signal.Notify(sigs, syscall.SIGINT, syscall.SIGTERM)
	sig := <-sigs
	cancel()
	if userURL == "" {
		return
	}
	wg.Wait()
	jsonFileName := al.SuggestedFileName + ".json"
	fmt.Printf("%v: marshalling state to %q\n", sig, jsonFileName)
	al.Location = userURL // preserve user provided url
	data, err := json.Marshal(al)
	if err != nil {
		log.Println(err)
		return
	}
	dst, err := os.Create(jsonFileName)
	if err != nil {
		log.Println(err)
		return
	}
	_, err = dst.Write(data)
	if errc := dst.Close(); err == nil {
		err = errc
	}
	logIfError(err)
}

func loadActualLocationFromJson(filename string) (*ActualLocation, error) {
	data, err := ioutil.ReadFile(filename)
	if err != nil {
		return nil, err
	}
	al := new(ActualLocation)
	err = json.Unmarshal(data, al)
	if err != nil {
		return nil, err
	}
	return al, nil
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

package main

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net/http"
	"net/url"
	"os"
	"regexp"
	"strings"
	"sync"

	"github.com/pkg/errors"
	"github.com/vbauerster/mpb"
)

const maxRedirects = 10

var (
	version              = "devel"
	contentDispositionRe *regexp.Regexp
)

func init() {
	// https://regex101.com/r/N4AovD/3
	contentDispositionRe = regexp.MustCompile(`filename[^;\n=]*=(['"](.*?)['"]|[^;\n]*)`)
}

func main() {
	userAgent := "Mozilla/5.0 (Macintosh; Intel Mac OS X 10_12_2) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/55.0.2883.95 Safari/537.36"
	url := "https://homebrew.bintray.com/bottles/libtiff-4.0.7.sierra.bottle.tar.gz"
	// url := "http://127.0.0.1:8080/libtiff-4.0.7.sierra.bottle.tar.gz"

	actualLocation, err := follow(parseURL(url))
	if err != nil {
		log.Fatal(err)
	}
	// fmt.Printf("actualLocation = %+v\n", actualLocation)

	totalParts := 2
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	if actualLocation.AcceptRanges == "bytes" && actualLocation.StatusCode == 206 {
		var wg sync.WaitGroup
		pb := mpb.New().SetWidth(60)
		partSize := actualLocation.ContentLength / 2
		actualLocation.Parts = make(map[int]*Part)
		for i := 0; i < totalParts; i++ {
			offset := int64(i) * partSize
			start, stop := offset, offset+partSize-1
			// name := fmt.Sprintf("%s.part%d", actualLocation.SuggestedFileName, i)
			name := fmt.Sprintf("%s.part%d", "test.tar.gz", i)
			part := &Part{
				Name:  name,
				Start: start,
				Stop:  stop,
			}
			actualLocation.Parts[i] = part
			wg.Add(1)
			go part.download(ctx, &wg, pb, url, userAgent, i)
		}
		wg.Wait()
		// fmt.Println("after wait")
		// pb.Stop()

		go func() {
			pb.Stop()
		}()

		fmt.Println("Writing...")
		f, err := os.OpenFile(actualLocation.Parts[0].Name, os.O_APPEND|os.O_WRONLY, 0644)
		exitOnError(err)
		defer f.Close()

		buf := make([]byte, 2048)
		for i := 1; i < totalParts; i++ {
			part, err := os.Open(actualLocation.Parts[i].Name)
			exitOnError(err)
			for {
				n, err := part.Read(buf[0:])
				_, errWrite := f.Write(buf[0:n])
				exitOnError(errWrite)
				if err != nil {
					if err == io.EOF {
						break
					}
					exitOnError(err)
				}
			}
			part.Close()
		}

	}

	data, err := json.MarshalIndent(actualLocation, "", "	")
	if err != nil {
		log.Fatalf("JSON marshaling failed: %s", err)
	}
	fmt.Printf("%s\n", data)
}

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
	Name        string
	Start, Stop int64
	Done        bool
}

func (p *Part) download(ctx context.Context, wg *sync.WaitGroup, pb *mpb.Progress, url, userAgent string, n int) {
	defer wg.Done()
	name := fmt.Sprintf("part#%d:", n)
	req, err := http.NewRequest(http.MethodGet, url, nil)
	if err != nil {
		log.Printf("%s%v\n", name, err)
		return
	}
	req.Header.Set("User-Agent", userAgent)
	req.Header.Set("Range", fmt.Sprintf("bytes=%d-%d", p.Start, p.Stop))

	fmt.Fprintf(os.Stderr, "%s Range = %+v\n", name, req.Header.Get("Range"))

	resp, err := http.DefaultClient.Do(req.WithContext(ctx))
	if err != nil {
		log.Printf("%s %v\n", name, err)
		return
	}
	defer resp.Body.Close()

	fmt.Fprintf(os.Stderr, "%s resp.StatusCode = %+v\n", name, resp.StatusCode)

	if resp.StatusCode != 206 {
		return
	}

	dest, err := os.Create(p.Name)
	if err != nil {
		log.Printf("%s %v\n", name, err)
		return
	}

	bar := pb.AddBar(int(p.Stop-p.Start)+1).
		PrependName(name, 0).
		PrependCounters(mpb.UnitBytes, 20).
		AppendETA(-6)

	// create proxy reader
	reader := bar.ProxyReader(resp.Body)
	// and copy from reader
	written, err := io.Copy(dest, reader)

	if closeErr := dest.Close(); err == nil {
		p.Done = true
		fmt.Fprintf(os.Stderr, "%s written = %d\n", name, written)
		err = closeErr
	}
	if err != nil {
		log.Printf("%s %v\n", name, err)
	}
}

func follow(url *url.URL) (*ActualLocation, error) {
	client := &http.Client{
		CheckRedirect: func(_ *http.Request, _ []*http.Request) error {
			return http.ErrUseLastResponse
		},
	}
	next := url.String()
	var actualLocation *ActualLocation
	var redirectsFollowed int
	for {
		resp, err := getResp(client, next)
		if err != nil {
			return nil, err
		}

		actualLocation = &ActualLocation{
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
	return actualLocation, nil
}

func getResp(client *http.Client, url string) (*http.Response, error) {
	req, err := http.NewRequest(http.MethodGet, url, nil)
	if err != nil {
		return nil, errors.Wrapf(err, "cannot make request with %q", url)
	}
	req.Header.Set("Range", "bytes=0-")
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

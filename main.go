package main

import (
	"flag"
	"fmt"
	"log"
	"net/http"
	"net/url"
	"regexp"
	"strings"

	"github.com/pkg/errors"
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
	flag.Parse()
	url2 := "https://homebrew.bintray.com/bottles/libtiff-4.0.7.sierra.bottle.tar.gz"

	actualLocation, err := follow(parseURL(url2))
	if err != nil {
		log.Fatal(err)
	}
	if actualLocation.StatusCode != http.StatusOK {
		log.Fatalf("non-200 status: %s", actualLocation.StatusCode)
	}
	fmt.Printf("actualLocation = %+v\n", actualLocation)
}

type ActualLocation struct {
	Location          string
	SuggestedFileName string
	ContentMD5        string
	AcceptRanges      string
	StatusCode        int
	ContentLength     int64
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

		filename, _ := parseContentDisposition(resp.Header.Get("Content-Disposition"))

		actualLocation = &ActualLocation{
			Location:          next,
			AcceptRanges:      resp.Header.Get("Accept-Ranges"),
			StatusCode:        resp.StatusCode,
			ContentLength:     resp.ContentLength,
			ContentMD5:        resp.Header.Get("Content-MD5"),
			SuggestedFileName: filename,
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
	resp, err := client.Get(url)
	if err != nil {
		return nil, errors.Wrapf(err, "cannot get %q", url)
	}
	defer resp.Body.Close()
	return resp, nil
}

func parseContentDisposition(input string) (string, error) {
	groups := contentDispositionRe.FindAllStringSubmatch(input, -1)
	if groups == nil {
		return "", errors.New("invalid Content-Disposition")
	}
	for _, group := range groups {
		if group[2] != "" {
			return group[2], nil
		}
		split := strings.Split(group[1], "'")
		if len(split) == 3 && strings.ToLower(split[0]) == "utf-8" {
			return url.QueryUnescape(split[2])
		}
		if split[0] != `""` {
			return split[0], nil
		}
	}
	return "", nil
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

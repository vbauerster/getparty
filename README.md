# getparty [![Build Status](https://travis-ci.org/vbauerster/getparty.svg?branch=master)](https://travis-ci.org/vbauerster/getparty)

HTTP Download Manager with multi-parts

![showcase](showcase.gif)

## Installation

#### Homebrew

```
$ brew tap vbauerster/getparty
$ brew install getparty
```

#### Manual

```
$ go get -u github.com/vbauerster/getparty
$ cd $GOPATH/src/github.com/vbauerster/getparty/cmd/getparty
$ go install
```

## Usage

```
Usage:
  getparty [OPTIONS] url

Application Options:
  -p, --parts=n                                             number of parts (default: 1)
  -r, --max-retry=n                                         max retry per each part, 0 for infinite (default: 10)
  -t, --timeout=sec                                         context timeout (default: 15)
  -o, --output=filename                                     user defined output
  -c, --continue=state.json                                 resume download from the last session
  -a, --user-agent=[chrome|firefox|safari|edge|getparty]    User-Agent header (default: chrome)
  -b, --best-mirror                                         pickup the fastest mirror
  -q, --quiet                                               quiet mode, no progress bars
  -f, --force                                               overwrite existing file silently
  -u, --username=                                           basic http auth username
      --password=                                           basic http auth password
  -H, --header=key:value                                    arbitrary http header
      --no-check-cert                                       don't validate the server's certificate
      --certs-file=certs.crt                                root certificates to use when verifying server certificates
      --debug                                               enable debug to stderr
      --version                                             show version

Help Options:
  -h, --help                                                show this help message
```

#### Best mirror example:

`cat` [mirrors.txt](https://github.com/vbauerster/getparty/blob/master/mirrors.txt) `| getparty -p 8 -b`

## License

[BSD 3-Clause](https://opensource.org/licenses/BSD-3-Clause)

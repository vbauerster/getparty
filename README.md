# getparty

[![Build status](https://github.com/vbauerster/getparty/actions/workflows/build.yml/badge.svg)](https://github.com/vbauerster/getparty/actions/workflows/build.yml)
[![Lint status](https://github.com/vbauerster/getparty/actions/workflows/golangci-lint.yml/badge.svg)](https://github.com/vbauerster/getparty/actions/workflows/golangci-lint.yml)

HTTP Download Manager with multi-parts

![showcase](showcase.gif)

## Installation

#### Homebrew

```
$ brew tap vbauerster/getparty
$ brew install getparty
```

#### Aur

```
$ paru -S getparty
```

#### From source

```
$ git clone --depth 1 https://github.com/vbauerster/getparty.git
$ cd getparty/cmd/getparty && go build
```

## Usage

```
Usage:
  getparty [OPTIONS] [<url>]

Application Options:
  -p, --parts=n                                             number of parts (default: 1)
  -r, --max-retry=n                                         max retries per each part, 0 for infinite (default: 10)
      --max-redirect=n                                      max redirections allowed, 0 for infinite (default: 10)
  -t, --timeout=sec                                         context timeout (default: 15)
  -l, --speed-limit=n                                       speed limit gauge, value from 1 to 10 inclusive
  -o, --output=FILE                                         output file name
  -s, --session=FILE                                        session state of incomplete download, file with json extension
  -U, --user-agent=[chrome|firefox|safari|edge|getparty]    User-Agent header (default: chrome)
      --username=                                           basic http auth username
      --password=                                           basic http auth password
  -H, --header=key:value                                    http header, can be specified more than once
  -f, --force                                               overwrite existing file silently
  -q, --quiet                                               quiet mode, no progress bars
  -d, --debug                                               enable debug to stderr
  -v, --version                                             show version

Https Options:
  -c, --certs-file=certs.crt                                root certificates to use when verifying server certificates
      --no-check-cert                                       don't verify the server's certificate chain and host name

Best-mirror Options:
  -m, --mirror.list=FILE|-                                  mirror list input
  -g, --mirror.max=n                                        max concurrent http request (default: number of logical CPUs)
      --mirror.top=n                                        list top n mirrors, download condition n=1 (default: 1)

Help Options:
  -h, --help                                                Show this help message

Arguments:
  <url>:                                                    http location
```

#### Best mirror example:

read [mirrors.txt](mirrors.txt) from a file and proceed to dowload from the best mirror:

```
getparty -m mirrors.txt
```

read [mirrors.txt](mirrors.txt) from a stdin and proceed to dowload from the best mirror:

```
cat mirrors.txt | getparty -m -
```

just list top 3 mirrors without any download:

```
getparty -m mirrors.txt --mirror.top 3
```

## License

[BSD 3-Clause](https://opensource.org/licenses/BSD-3-Clause)

# getparty

[![Build status](https://github.com/vbauerster/getparty/actions/workflows/build.yml/badge.svg)](https://github.com/vbauerster/getparty/actions/workflows/build.yml)

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
  getparty [OPTIONS] url

Application Options:
  -p, --parts=n                                             number of parts (default: 1)
  -r, --max-retry=n                                         max retry per each part, 0 for infinite (default: 10)
  -t, --timeout=sec                                         context timeout (default: 15)
  -l, --speed-limit=n                                       speed limit gauge, value from 1 to 10 inclusive
  -o, --output=filename                                     user defined output
  -s, --session=session.json                                path to saved session file (optional)
  -a, --user-agent=[chrome|firefox|safari|edge|getparty]    User-Agent header (default: chrome)
  -b, --best-mirror                                         pickup best mirror, repeat n times to list top n
  -q, --quiet                                               quiet mode, no progress bars
  -f, --force                                               overwrite existing file silently
  -u, --username=                                           basic http auth username
      --password=                                           basic http auth password
  -H, --header=key:value                                    http header, can be specified more than once
      --no-check-cert                                       don't validate the server's certificate
  -c, --certs-file=certs.crt                                root certificates to use when verifying server certificates
      --debug                                               enable debug to stderr
  -v, --version                                             show version

Help Options:
  -h, --help                                                Show this help message
```

#### Best mirror example:

either pipe to stdin:

```
cat mirrors.txt | getparty -b
```

or directly from a file:

```
getparty -b mirrors.txt
```

just list top 3 mirrors:

```
getparty -bbb mirrors.txt
```

## License

[BSD 3-Clause](https://opensource.org/licenses/BSD-3-Clause)

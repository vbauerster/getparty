# getparty [![Build Status](https://travis-ci.org/vbauerster/getparty.svg?branch=master)](https://travis-ci.org/vbauerster/getparty)

Console download manager

![showcase](showcase.gif)

## Installation
`getparty` requires Go 1.7 or later.
```
$ go install github.com/vbauerster/getparty/cmd/getparty
```

## Usage

```
Usage:
  getparty [OPTIONS] url

Application Options:
  -p, --parts=n                               number of parts (default: 2)
  -o, --output=filename                       user defined output
  -c, --continue=state                        resume download from the last saved state file
  -a, --user-agent=[chrome|firefox|safari]    User-Agent header (default: chrome)
  -b, --best-mirror                           pickup the fastest mirror, will read from stdin
  -u, --username=                             basic http auth username
      --password=                             basic http auth password
      --debug                                 enable debug to stderr
      --version                               show version

Help Options:
  -h, --help                                  Show this help message
```

#### Best mirror example:

`cat` [mirrors.txt](https://github.com/vbauerster/getparty/blob/master/mirrors.txt) `| getparty -p 8 -b`

## License

[BSD 3-Clause](https://opensource.org/licenses/BSD-3-Clause)

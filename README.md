# getparty [![Build Status](https://travis-ci.org/vbauerster/getparty.svg?branch=master)](https://travis-ci.org/vbauerster/getparty)

Console download manager

![showcase](showcase.gif)

## [Installation](https://github.com/vbauerster/getparty/releases)
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
  -p, --parts=n                               number of parts (default: 2)
  -t, --timeout=sec                           context timeout (default: 15)
  -o, --output=filename                       user defined output
  -c, --continue=state.json                   resume download from the last session
  -a, --user-agent=[chrome|firefox|safari]    User-Agent header (default: chrome)
  -b, --best-mirror                           pickup the fastest mirror
  -q, --quiet                                 quiet mode, no progress bars
  -u, --username=                             basic http auth username
      --password=                             basic http auth password
      --header=key:value                      arbitrary http header
      --debug                                 enable debug to stderr
      --version                               show version

Help Options:
  -h, --help                                  Show this help message
```

#### Best mirror example:
`cat` [mirrors.txt](https://github.com/vbauerster/getparty/blob/master/mirrors.txt) `| getparty -p 8 -b`

## License
[BSD 3-Clause](https://opensource.org/licenses/BSD-3-Clause)

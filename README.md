# getparty [![Build Status](https://travis-ci.org/vbauerster/getparty.svg?branch=master)](https://travis-ci.org/vbauerster/getparty)

Console download manager

![showcase](showcase.gif)

## Installation
`getparty` requires Go 1.7.1 or later.
```
$ go get -u github.com/vbauerster/getparty
```
Or download [binary](https://github.com/vbauerster/getparty/releases/latest).

## Usage

```
Usage:
  getparty [OPTIONS] url

Application Options:
  -c, --continue=state.json     resume download from last saved json state
  -m, --best-mirror=list.txt    pickup the fastest mirror from provided list
  -p, --parts=                  number of parts (default: 2)
  -t, --timeout=                download timeout in seconds
      --version                 show version

Help Options:
  -h, --help                    Show this help message
```

#### Best mirror example:

`getparty -p 8 -m` [mirrors.txt](https://github.com/vbauerster/getparty/blob/master/mirrors.txt)

## License

[MIT](https://github.com/vbauerster/getparty/blob/master/LICENSE)

## Author

[vbauerster](https://github.com/vbauerster)


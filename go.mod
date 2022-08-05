module github.com/vbauerster/getparty

require (
	github.com/VividCortex/ewma v1.2.0
	github.com/hashicorp/go-cleanhttp v0.5.2
	github.com/jessevdk/go-flags v1.5.0
	github.com/pkg/errors v0.9.1
	github.com/vbauerster/backoff v0.0.0-20220115105225-8ab11faa2b99
	github.com/vbauerster/mpb/v7 v7.4.2
	golang.org/x/net v0.0.0-20220520000938-2e3eb7b945c2
	golang.org/x/sync v0.0.0-20220513210516-0976fa681c29
	golang.org/x/term v0.0.0-20220411215600-e5f449aeb171
)

replace github.com/vbauerster/mpb/v7 => /home/vbauer/gohack/github.com/vbauerster/mpb/v7

go 1.14

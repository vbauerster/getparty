VERSION = $(shell git describe --tags)
TARGETS = linux_386 linux_amd64 linux_arm linux_arm64 darwin_amd64 windows_386 windows_amd64
COMMAND_NAME = getparty
PACKAGE_NAME = github.com/vbauerster/$(COMMAND_NAME)/cmd/$(COMMAND_NAME)
LDFLAGS = -ldflags=-X=main.version=$(VERSION)
OBJECTS = $(patsubst $(COMMAND_NAME)%_windows_amd64,$(COMMAND_NAME)%_windows_amd64.exe, $(patsubst $(COMMAND_NAME)%_windows_386,$(COMMAND_NAME)%_windows_386.exe, $(patsubst %,$(COMMAND_NAME)_$(VERSION)_%, $(TARGETS))))

release: $(OBJECTS) ## Build release binaries

clean:
	rm -rf bin

$(OBJECTS): $(wildcard *.go)
	env GOOS=`echo $@ | cut -d'_' -f3` GOARCH=`echo $@ | cut -d'_' -f4 | cut -d'.' -f 1` go build -o bin/$@ $(LDFLAGS) $(PACKAGE_NAME)

.PHONY: help

help:
	@grep -E '^[a-zA-Z_-]+:.*?## .*$$' $(MAKEFILE_LIST) | sort | awk 'BEGIN {FS = ":.*?## "}; {printf "\033[36m%-30s\033[0m %s\n", $$1, $$2}'

.DEFAULT_GOAL := help

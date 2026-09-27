# Makefile for antigravity-cli customizations and tools

SHELL := /bin/bash
GO ?= go

.PHONY: all statusbar clean

all: statusbar

# Build the statusbar binary
statusbar: statusbar.go
	$(GO) build -ldflags="-s -w" -o statusbar statusbar.go
	@chmod +x statusbar

clean:
	rm -f statusbar

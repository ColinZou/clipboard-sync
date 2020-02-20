#!/usr/bin/env bash
OUTPUT_DIR=`dirname $0`/bin
mkdir -p $OUTPUT_DIR
#GOOS=windows GOARCH=amd64 go build -ldflags -H=windowsgui -o $OUTPUT_DIR/clipboard-sync.client.exe clipboard-sync.go
GOOS=windows GOARCH=amd64 go build -o $OUTPUT_DIR/clipboard-sync.client.exe clipboard-sync.go
GOOS=linux GOARCH=amd64 go build -o $OUTPUT_DIR/clipboard-sync.server clipboard-sync.go

cp $OUTPUT_DIR/clipboard-sync.server ~/Bin

package main

import (
	"context"
	"flag"
	"fmt"
	"minimalharness/internal/release"
	"os"
	"strings"
)

func main() {
	targets := flag.String("targets", "linux/amd64,linux/arm64,darwin/amd64,darwin/arm64,windows/amd64", "comma-separated release targets")
	version := flag.String("version", "dev", "release version")
	out := flag.String("out", "dist", "archive directory")
	flag.Parse()
	for _, raw := range strings.Split(*targets, ",") {
		t, err := release.ParseTarget(raw)
		if err != nil {
			fail(err)
		}
		path, err := release.Package(context.Background(), t, *version, *out, nil)
		if err != nil {
			fail(err)
		}
		fmt.Println(path)
	}
}
func fail(err error) { fmt.Fprintln(os.Stderr, err); os.Exit(1) }

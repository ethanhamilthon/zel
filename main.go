package main

import (
	"context"
	_ "embed"
	"fmt"
	"os"
	"os/signal"
	"strings"
	"syscall"

	"zel/internal/cli"
	"zel/internal/core"
	"zel/internal/tui"
)

//go:embed VERSION
var version string

func main() {
	cli.Version = strings.TrimSpace(version)
	tui.Version = cli.Version
	ctx, cancel := context.Background(), func() {}
	if !cli.IsHelper(os.Args[1:]) {
		// Sessions save and clean up on a signal; helpers just end, even when blocked reading.
		ctx, cancel = signal.NotifyContext(ctx, os.Interrupt, syscall.SIGTERM)
	}
	defer cancel()
	if err := cli.RunProcess(ctx, os.Args[1:], cli.Options{Open: core.New, Interactive: tui.Run}); err != nil {
		code := cli.ExitCode(err)
		if !cli.IsReported(err) {
			fmt.Fprintln(os.Stderr, err)
		}
		os.Exit(code)
	}
}

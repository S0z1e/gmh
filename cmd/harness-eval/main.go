// harness-eval defaults to deterministic fixtures. Live calls require explicit
// configuration, a pinned model, and suite-wide token, time and USD budgets.
package main

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"minimalharness/internal/eval"
	"os"
	"os/signal"
	"path/filepath"
	"time"
)

func main() { os.Exit(run()) }
func run() int {
	root := flag.String("root", ".", "repository checkout containing the fixed scenario suite")
	liveConfig := flag.String("live-config", "", "explicit config path: opt in to real model calls")
	model := flag.String("model", "", "pinned model selector for live evaluation")
	cost := flag.Float64("max-cost-usd", 0, "live suite estimated USD cap; requires complete rates")
	tokens := flag.Int64("max-tokens", 0, "live suite reported token cap")
	duration := flag.Duration("timeout", 20*time.Minute, "whole suite deadline")
	flag.Parse()
	if *duration <= 0 {
		fmt.Fprintln(os.Stderr, "timeout must be positive")
		return 2
	}
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt)
	defer stop()
	ctx, cancel := context.WithTimeout(ctx, *duration)
	defer cancel()
	var report eval.Report
	var err error
	if *liveConfig != "" {
		config, absErr := filepath.Abs(*liveConfig)
		if absErr != nil {
			fmt.Fprintln(os.Stderr, absErr)
			return 2
		}
		options := eval.LiveOptions{Config: config, Model: *model, MaxCostUSD: *cost, MaxTokens: *tokens}
		if err := options.Validate(); err != nil {
			fmt.Fprintln(os.Stderr, err)
			return 2
		}
		// Only this dedicated CLI process changes HOME; original credentials remain
		// available through the explicit config/env, while sessions stay temporary.
		home, mkdirErr := os.MkdirTemp("", "harness-eval-home-*")
		if mkdirErr != nil {
			fmt.Fprintln(os.Stderr, mkdirErr)
			return 1
		}
		defer os.RemoveAll(home)
		if err := os.Setenv("HOME", home); err != nil {
			fmt.Fprintln(os.Stderr, err)
			return 1
		}
		if err := os.Setenv("USERPROFILE", home); err != nil {
			fmt.Fprintln(os.Stderr, err)
			return 1
		}
		report, err = eval.RunLive(ctx, *root, options)
	} else {
		if *model != "" || *cost != 0 || *tokens != 0 {
			fmt.Fprintln(os.Stderr, "live model/budget flags require -live-config")
			return 2
		}
		report, err = eval.Run(ctx, *root)
	}
	enc := json.NewEncoder(os.Stdout)
	enc.SetIndent("", "  ")
	if encodeErr := enc.Encode(report); encodeErr != nil {
		fmt.Fprintln(os.Stderr, encodeErr)
		return 1
	}
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		return 1
	}
	return 0
}

package main

import (
	"context"
	"flag"
	"fmt"
	"io"
	"log/slog"
	"os"
	"os/signal"
	"syscall"

	"github.com/kohii/gitloop/internal/config"
	"github.com/kohii/gitloop/internal/daemon"
)

func runCmd(args []string, stdout, stderr io.Writer) int {
	fs := flag.NewFlagSet("run", flag.ContinueOnError)
	fs.SetOutput(stderr)
	configPath := fs.String("config", defaultConfigPathOrEmpty(), "path to the gitloop config file")
	if err := fs.Parse(args); err != nil {
		return 2
	}
	if *configPath == "" {
		fmt.Fprintln(stderr, "gitloop run: -config is required (could not determine a default)")
		return 2
	}

	cfg, err := config.Load(*configPath)
	if err != nil {
		fmt.Fprintf(stderr, "gitloop run: %v\n", err)
		return 1
	}

	logger := slog.New(slog.NewTextHandler(stderr, &slog.HandlerOptions{Level: slog.LevelInfo}))
	d, err := daemon.New(cfg, daemon.WithLogger(logger))
	if err != nil {
		fmt.Fprintf(stderr, "gitloop run: %v\n", err)
		return 1
	}

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	logger.Info("gitloop starting", "repositories", len(cfg.Repositories), "config", *configPath)
	if err := d.Run(ctx); err != nil {
		fmt.Fprintf(stderr, "gitloop run: %v\n", err)
		return 1
	}
	logger.Info("gitloop stopped")
	return 0
}

// defaultConfigPathOrEmpty returns config.DefaultPath(), or "" if the home
// directory can't be resolved (flag default values can't return errors).
func defaultConfigPathOrEmpty() string {
	p, err := config.DefaultPath()
	if err != nil {
		return ""
	}
	return p
}

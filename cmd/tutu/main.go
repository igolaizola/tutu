package main

import (
	"context"
	"log"
	"os"
	"os/signal"
	"runtime/debug"
	"syscall"

	"github.com/igolaizola/tutu"
	"github.com/igolaizola/tutu/internal/flags"
)

// version can provided on build time, ex.: `-ldflags "-X main.version=v0.0.0"`
var version = ""

func main() {
	// create context and listen interruptions
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	// Create logger
	logger := &levelLogger{}

	// listen interruptions
	exit := make(chan os.Signal, 1)
	signal.Notify(exit, syscall.SIGINT, syscall.SIGTERM)
	go func() {
		select {
		case <-ctx.Done():
		case <-exit:
			logger.Info("shutting down...")
			cancel()
		}
		signal.Stop(exit)
	}()

	// Obtain version
	v := version
	if v == "" {
		if buildInfo, ok := debug.ReadBuildInfo(); ok {
			v = buildInfo.Main.Version
		}
	}
	if v == "" {
		v = "dev"
	}

	// Parse configuration from command line and config file
	configs, verbose, err := flags.Parse(v)
	if err != nil {
		logger.Info(err.Error())
		return
	}
	logger.verbose = verbose

	// If only one stream has been configured add a default stdio
	if len(configs) == 1 {
		number := configs[0].Number + 1
		configs = append(configs, &tutu.Config{
			Number: number,
			Type:   "stdio",
		})
	}

	// Run pipe
	if err := tutu.Pipe(ctx, logger, configs...); err != nil {
		if ctx.Err() != nil {
			return
		}
		logger.Info(err.Error())
	}
}

type levelLogger struct {
	verbose bool
}

func (l *levelLogger) Info(msg string) {
	log.Println("[INFO]", msg)
}

func (l *levelLogger) Debug(msg string) {
	if !l.verbose {
		return
	}
	log.Println("[DEBUG]", msg)
}

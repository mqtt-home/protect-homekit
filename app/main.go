package main

import (
	"net/http"
	_ "net/http/pprof"
	"os"
	"os/signal"
	"path/filepath"
	"strconv"
	"syscall"

	"github.com/philipparndt/go-logger"
	"github.com/mqtt-home/protect-homekit/bridge"
	"github.com/mqtt-home/protect-homekit/config"
	"github.com/mqtt-home/protect-homekit/version"
)

func main() {
	logger.Init("info", logger.Logger())
	logger.Info("protect-homekit", "version", version.Info())

	if len(os.Args) < 2 {
		logger.Error("No configuration file specified")
		os.Exit(1)
	}

	configFile := os.Args[1]
	logger.Info("Configuration file", "path", configFile)

	cfg, err := config.LoadConfig(configFile)
	if err != nil {
		logger.Error("Failed to load configuration", "error", err)
		os.Exit(1)
	}
	logger.SetLevel(cfg.LogLevel)

	// Persisted pairing state lives next to the config file unless overridden.
	if cfg.HomeKit.StorageDir == "" {
		cfg.HomeKit.StorageDir = filepath.Join(filepath.Dir(configFile), "hap")
	}

	initPprof(cfg.Pprof)

	b := bridge.New(cfg)
	if err := b.Start(); err != nil {
		logger.Error("Failed to start bridge", "error", err)
		os.Exit(1)
	}

	logger.Info("Application ready")

	quitChannel := make(chan os.Signal, 1)
	signal.Notify(quitChannel, syscall.SIGINT, syscall.SIGTERM)
	<-quitChannel

	b.Stop()
	logger.Info("Shutdown complete")
}

// initPprof serves the Go pprof endpoint when enabled in the config. The
// net/http/pprof import registers its handlers on the default mux, which only
// this listener serves.
func initPprof(cfg config.PprofConfig) {
	if !cfg.Enabled {
		return
	}
	addr := cfg.Bind + ":" + strconv.Itoa(cfg.Port)
	logger.Info("pprof profiling enabled", "address", addr)
	go func() {
		if err := http.ListenAndServe(addr, nil); err != nil {
			logger.Error("pprof server stopped", "error", err)
		}
	}()
}

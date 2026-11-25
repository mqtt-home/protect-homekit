package main

import (
	"net/http"
	_ "net/http/pprof"
	"os"
	"os/signal"
	"path/filepath"
	"strconv"
	"syscall"

	"github.com/mqtt-home/protect-homekit/bridge"
	"github.com/mqtt-home/protect-homekit/config"
	"github.com/mqtt-home/protect-homekit/mqttpub"
	"github.com/mqtt-home/protect-homekit/version"
	"github.com/mqtt-home/protect-homekit/web"
	"github.com/philipparndt/go-logger"
	gwconfig "github.com/philipparndt/mqtt-gateway/config"
	"github.com/philipparndt/mqtt-gateway/mqtt"
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

	var listeners []func(bridge.CameraInfo)

	var webServer *web.WebServer
	if cfg.Web.Enabled {
		webServer = web.NewWebServer(b, cfg.Web)
		listeners = append(listeners, webServer.BroadcastCamera)
	}

	var publisher *mqttpub.Publisher
	if cfg.MQTT.Enabled() {
		mqtt.Start(gwconfig.MQTTConfig{
			URL:      cfg.MQTT.URL,
			Username: cfg.MQTT.Username,
			Password: cfg.MQTT.Password,
			Topic:    cfg.MQTT.Topic,
			QoS:      cfg.MQTT.QoS,
			Retain:   true,
		}, "protect_homekit")
		publisher = mqttpub.New(cfg.MQTT.Topic, mqtt.PublishAbsolute)
		listeners = append(listeners, publisher.OnCamera)
		logger.Info("Publishing camera state to MQTT", "topic", cfg.MQTT.Topic)
	}

	b.SetUpdateListener(func(info bridge.CameraInfo) {
		for _, l := range listeners {
			l(info)
		}
	})

	if err := b.Start(); err != nil {
		logger.Error("Failed to start bridge", "error", err)
		os.Exit(1)
	}

	if webServer != nil {
		go func() {
			port := cfg.Web.Port
			logger.Info("Web interface available", "url", "http://localhost:"+strconv.Itoa(port))
			if err := webServer.Start(port); err != nil {
				logger.Error("Failed to start web server", "error", err)
			}
		}()
	}

	// Retained state for every camera, also for the ones that stay quiet.
	if publisher != nil {
		for _, info := range b.CameraInfos() {
			publisher.OnCamera(info)
		}
	}

	logger.Info("Application ready")

	quitChannel := make(chan os.Signal, 1)
	signal.Notify(quitChannel, syscall.SIGINT, syscall.SIGTERM)
	<-quitChannel

	b.Stop()
	if publisher != nil {
		publisher.Close()
		mqtt.Stop()
	}
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

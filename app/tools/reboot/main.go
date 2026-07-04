// Command reboot restarts a Protect camera by name (diagnostics; e.g. to
// apply a video codec switch to a camera that keeps streaming the old codec).
package main

import (
	"fmt"
	"os"

	"github.com/mqtt-home/protect-homekit/config"
	"github.com/mqtt-home/protect-homekit/protect"
	"github.com/philipparndt/go-logger"
)

func main() {
	logger.Init("info", logger.Logger())
	if len(os.Args) < 3 {
		fmt.Fprintln(os.Stderr, "usage: reboot <config.yaml> <camera-name>")
		os.Exit(1)
	}

	cfg, err := config.LoadConfig(os.Args[1])
	if err != nil {
		logger.Error("load config", "error", err)
		os.Exit(1)
	}

	client := protect.NewClient(cfg.Protect.Host, cfg.Protect.Username, cfg.Protect.Password, cfg.Protect.VerifySSL)
	if err := client.Login(); err != nil {
		logger.Error("login", "error", err)
		os.Exit(1)
	}
	bs, err := client.Bootstrap()
	if err != nil {
		logger.Error("bootstrap", "error", err)
		os.Exit(1)
	}

	for i := range bs.Cameras {
		if bs.Cameras[i].Name == os.Args[2] {
			if err := client.Reboot(&bs.Cameras[i]); err != nil {
				logger.Error("reboot", "error", err)
				os.Exit(1)
			}
			return
		}
	}
	fmt.Fprintf(os.Stderr, "camera %q not found\n", os.Args[2])
	os.Exit(1)
}

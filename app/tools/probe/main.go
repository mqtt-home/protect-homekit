// Command probe is a diagnostic helper: it logs in to Protect, lists all
// cameras with their channels and prints the RTSPS URLs, so streams can be
// inspected with ffprobe/ffplay outside of HomeKit.
package main

import (
	"fmt"
	"os"

	"github.com/philipparndt/go-logger"
	"github.com/mqtt-home/protect-homekit/config"
	"github.com/mqtt-home/protect-homekit/protect"
)

func main() {
	logger.Init("info", logger.Logger())

	if len(os.Args) < 2 {
		fmt.Fprintln(os.Stderr, "usage: probe <config.yaml>")
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

	fmt.Printf("NVR: %s (%s), rtsps port %d\n\n", bs.NVR.Name, bs.NVR.Version, bs.NVR.Ports.RTSPS)
	for _, cam := range bs.Cameras {
		fmt.Printf("%s  [%s]  state=%s doorbell=%v mic=%v\n", cam.Name, cam.Type, cam.State, cam.IsDoorbell(), cam.FeatureFlags.HasMic)
		for _, ch := range cam.Channels {
			url := ""
			if ch.RtspAlias != "" {
				url = client.RTSPSURL(bs, ch.RtspAlias)
			}
			fmt.Printf("  ch%d %-7s enabled=%-5v rtsp=%-5v %4dx%-4d %2dfps idr=%ds bitrate=%d  %s\n",
				ch.ID, ch.Name, ch.Enabled, ch.IsRtspEnabled, ch.Width, ch.Height, ch.FPS, ch.IDRInterval, ch.Bitrate, url)
		}
		fmt.Println()
	}
}

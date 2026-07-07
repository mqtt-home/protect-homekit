package web

import (
	"encoding/json"
	"fmt"
	"log/slog"
	"net/http"
	"runtime"
	"strconv"
	"sync"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/go-chi/chi/v5/middleware"
	"github.com/go-chi/cors"
	"github.com/mqtt-home/protect-homekit/bridge"
	"github.com/mqtt-home/protect-homekit/config"
	"github.com/philipparndt/go-logger"
	loggerchi "github.com/philipparndt/go-logger/chi"
	qrcode "github.com/skip2/go-qrcode"
)

type sseClient struct {
	id      string
	channel chan string
}

// WebServer exposes a REST + SSE API and the static SPA for the bridge.
type WebServer struct {
	b   *bridge.Bridge
	cfg config.WebConfig

	router *chi.Mux

	sseClientsMu sync.RWMutex
	sseClients   map[string]*sseClient

	unhealthySince *time.Time
}

func NewWebServer(b *bridge.Bridge, cfg config.WebConfig) *WebServer {
	ws := &WebServer{
		b:          b,
		cfg:        cfg,
		router:     chi.NewRouter(),
		sseClients: map[string]*sseClient{},
	}
	ws.setupRoutes()
	return ws
}

func (ws *WebServer) livenessGrace() time.Duration {
	if s := ws.cfg.LivenessGraceSeconds; s > 0 {
		return time.Duration(s) * time.Second
	}
	return 4 * time.Minute
}

func (ws *WebServer) setupRoutes() {
	ws.router.Use(loggerchi.LoggerWithLevel(slog.LevelDebug))
	ws.router.Use(middleware.Recoverer)
	ws.router.Use(cors.Handler(cors.Options{
		AllowedOrigins: []string{"*"},
		AllowedMethods: []string{"GET", "OPTIONS"},
		AllowedHeaders: []string{"Accept", "Content-Type"},
		MaxAge:         300,
	}))

	ws.router.Route("/api", func(r chi.Router) {
		r.Get("/health", ws.health)
		r.Get("/livez", ws.liveness)
		r.Get("/info", ws.info)
		r.Get("/cameras", ws.cameras)
		r.Get("/cameras/{id}/snapshot", ws.snapshot)
		r.Get("/cameras/{id}/live", ws.live)
		r.Get("/cameras/{id}/qr", ws.cameraQR)
		r.Get("/qr", ws.qr)
		r.Get("/events", ws.handleSSE)
	})

	// SPA: serve static files, fall back to index.html for client-side routes.
	distDir := "./web/dist/"
	fileServer := http.FileServer(http.Dir(distDir))
	ws.router.HandleFunc("/*", func(w http.ResponseWriter, r *http.Request) {
		path := "." + r.URL.Path
		if _, err := http.Dir(distDir).Open(path); err == nil {
			fileServer.ServeHTTP(w, r)
			return
		}
		http.ServeFile(w, r, distDir+"index.html")
	})
}

func (ws *WebServer) info(w http.ResponseWriter, _ *http.Request) {
	nvrName, nvrVersion := ws.b.NVRInfo()
	writeJSON(w, map[string]any{
		"bridge":      ws.b.BridgeName(),
		"pin":         ws.b.Pin(),
		"setup_id":    ws.b.SetupID(),
		"setup_uri":   ws.b.SetupURI(),
		"standalone":  ws.b.Standalone(),
		"nvr":         nvrName,
		"nvr_version": nvrVersion,
		"cameras":     len(ws.b.CameraInfos()),
		"healthy":     ws.b.Healthy(),
	})
}

func (ws *WebServer) cameras(w http.ResponseWriter, _ *http.Request) {
	writeJSON(w, ws.b.CameraInfos())
}

// snapshot proxies a camera JPEG from the NVR (shared snapshot cache).
func (ws *WebServer) snapshot(w http.ResponseWriter, r *http.Request) {
	id := chi.URLParam(r, "id")
	width, _ := strconv.Atoi(r.URL.Query().Get("w"))
	height, _ := strconv.Atoi(r.URL.Query().Get("h"))

	data, err := ws.b.CameraSnapshot(id, width, height)
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadGateway)
		return
	}
	w.Header().Set("Content-Type", "image/jpeg")
	w.Header().Set("Cache-Control", "no-store")
	_, _ = w.Write(data)
}

// qr renders the HomeKit pairing QR code as a PNG.
func (ws *WebServer) qr(w http.ResponseWriter, _ *http.Request) {
	png, err := qrcode.Encode(ws.b.SetupURI(), qrcode.Medium, 320)
	if err != nil {
		http.Error(w, "qr error", http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", "image/png")
	w.Header().Set("Cache-Control", "no-store")
	_, _ = w.Write(png)
}

// cameraQR renders the pairing QR code for a single standalone camera.
func (ws *WebServer) cameraQR(w http.ResponseWriter, r *http.Request) {
	uri, ok := ws.b.CameraSetupURI(chi.URLParam(r, "id"))
	if !ok {
		http.Error(w, "camera not pairable (bridged mode)", http.StatusNotFound)
		return
	}
	png, err := qrcode.Encode(uri, qrcode.Medium, 320)
	if err != nil {
		http.Error(w, "qr error", http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", "image/png")
	w.Header().Set("Cache-Control", "no-store")
	_, _ = w.Write(png)
}

func (ws *WebServer) health(w http.ResponseWriter, _ *http.Request) {
	writeJSON(w, map[string]any{
		"status":     "ok",
		"goroutines": runtime.NumGoroutine(),
		"cameras":    len(ws.b.CameraInfos()),
		"healthy":    ws.b.Healthy(),
		"timestamp":  time.Now().UTC().Format(time.RFC3339),
	})
}

func (ws *WebServer) liveness(w http.ResponseWriter, _ *http.Request) {
	statusCode, newSince, stuckFor := evaluateLiveness(ws.b.Healthy(), ws.unhealthySince, time.Now(), ws.livenessGrace())
	ws.unhealthySince = newSince
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(statusCode)
	_ = json.NewEncoder(w).Encode(map[string]any{
		"healthy":         ws.b.Healthy(),
		"stuckForSeconds": int(stuckFor.Seconds()),
		"timestamp":       time.Now().UTC().Format(time.RFC3339),
	})
}

// evaluateLiveness fails (503) only after the bridge has been continuously
// unhealthy for longer than the grace window.
func evaluateLiveness(healthy bool, unhealthySince *time.Time, now time.Time, grace time.Duration) (int, *time.Time, time.Duration) {
	if healthy {
		return http.StatusOK, nil, 0
	}
	if unhealthySince == nil {
		t := now
		unhealthySince = &t
	}
	stuckFor := now.Sub(*unhealthySince)
	code := http.StatusOK
	if stuckFor > grace {
		code = http.StatusServiceUnavailable
	}
	return code, unhealthySince, stuckFor
}

// --- SSE ---

// BroadcastCamera pushes a camera state update to all connected SSE clients.
func (ws *WebServer) BroadcastCamera(info bridge.CameraInfo) {
	message, err := json.Marshal(info)
	if err != nil {
		return
	}
	ws.broadcast(string(message))
}

func (ws *WebServer) broadcast(msg string) {
	ws.sseClientsMu.RLock()
	for _, c := range ws.sseClients {
		select {
		case c.channel <- msg:
		default:
		}
	}
	ws.sseClientsMu.RUnlock()
}

func (ws *WebServer) handleSSE(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("Connection", "keep-alive")
	w.Header().Set("Access-Control-Allow-Origin", "*")

	clientID := fmt.Sprintf("%d", time.Now().UnixNano())
	channel := make(chan string, 16)

	ws.sseClientsMu.Lock()
	ws.sseClients[clientID] = &sseClient{id: clientID, channel: channel}
	ws.sseClientsMu.Unlock()

	// Initial snapshot of all cameras.
	for _, info := range ws.b.CameraInfos() {
		if msg, err := json.Marshal(info); err == nil {
			fmt.Fprintf(w, "data: %s\n\n", string(msg))
		}
	}
	flusher, ok := w.(http.Flusher)
	if ok {
		flusher.Flush()
	}

	defer func() {
		ws.sseClientsMu.Lock()
		delete(ws.sseClients, clientID)
		close(channel)
		ws.sseClientsMu.Unlock()
	}()

	for {
		select {
		case msg := <-channel:
			if _, err := fmt.Fprintf(w, "data: %s\n\n", msg); err != nil {
				return
			}
			if ok {
				flusher.Flush()
			}
		case <-r.Context().Done():
			return
		}
	}
}

func (ws *WebServer) Start(port int) error {
	addr := ":" + strconv.Itoa(port)
	logger.Info("Starting web server", "address", addr)
	return http.ListenAndServe(addr, ws.router)
}

func writeJSON(w http.ResponseWriter, v any) {
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(v)
}

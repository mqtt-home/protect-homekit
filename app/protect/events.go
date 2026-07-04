package protect

import (
	"context"
	"crypto/tls"
	"encoding/json"
	"net/http"
	"time"

	"github.com/gorilla/websocket"
	"github.com/philipparndt/go-logger"
)

// pongWait is how long the read loop waits for any traffic before treating
// the connection as dead. Protect pushes updates continuously, but an idle
// system may be quiet, so pings keep the deadline refreshed.
const pongWait = 60 * time.Second

// pingPeriod must be shorter than pongWait so pongs refresh the deadline in
// time.
const pingPeriod = 25 * time.Second

const writeWait = 10 * time.Second

// reconnectDelay is the pause between reconnect attempts.
const reconnectDelay = 5 * time.Second

// Events maintains the realtime updates websocket. On every (re)connect it
// logs in and re-bootstraps, so listeners always resync to fresh state and the
// websocket attaches with a current lastUpdateId.
type Events struct {
	client *Client

	// OnBootstrap is called after every successful bootstrap, before the
	// websocket starts delivering updates.
	OnBootstrap func(*Bootstrap)
	// OnCameraUpdate is called for partial camera updates (motion, ring,
	// connection state).
	OnCameraUpdate func(cameraID string, patch CameraPatch)
}

func NewEvents(client *Client) *Events {
	return &Events{client: client}
}

// Run connects and keeps the event stream alive until ctx is canceled.
// The initial bootstrap can be passed in to avoid fetching it twice at
// startup.
func (e *Events) Run(ctx context.Context, initial *Bootstrap) {
	bs := initial
	for {
		if bs == nil {
			var err error
			if err = e.client.Login(); err == nil {
				bs, err = e.client.Bootstrap()
			}
			if err != nil {
				logger.Error("Protect reconnect failed", "error", err)
				if !sleepCtx(ctx, reconnectDelay) {
					return
				}
				continue
			}
			if e.OnBootstrap != nil {
				e.OnBootstrap(bs)
			}
		}

		err := e.listen(ctx, bs)
		bs = nil // force re-login + re-bootstrap on the next iteration
		if ctx.Err() != nil {
			return
		}
		logger.Warn("Protect event stream disconnected, reconnecting", "error", err)
		if !sleepCtx(ctx, reconnectDelay) {
			return
		}
	}
}

// listen connects the websocket and dispatches messages until the connection
// dies or ctx is canceled.
func (e *Events) listen(ctx context.Context, bs *Bootstrap) error {
	dialer := websocket.Dialer{
		TLSClientConfig:  &tls.Config{InsecureSkipVerify: !e.client.VerifySSL()},
		HandshakeTimeout: 30 * time.Second,
	}

	headers := http.Header{}
	if cookie := e.client.CookieHeader(); cookie != "" {
		headers.Set("Cookie", cookie)
	}

	wsURL := e.client.UpdatesWebsocketURL(bs.LastUpdateID)
	conn, resp, err := dialer.Dial(wsURL, headers)
	if err != nil {
		if resp != nil {
			logger.Error("Protect websocket handshake failed", "status", resp.StatusCode)
		}
		return err
	}
	defer conn.Close()
	logger.Info("Connected to Protect realtime updates")

	_ = conn.SetReadDeadline(time.Now().Add(pongWait))
	conn.SetPongHandler(func(string) error {
		return conn.SetReadDeadline(time.Now().Add(pongWait))
	})

	// Close the connection when ctx ends so ReadMessage unblocks; the ping
	// loop doubles as dead-connection detection.
	done := make(chan struct{})
	defer close(done)
	go func() {
		ticker := time.NewTicker(pingPeriod)
		defer ticker.Stop()
		for {
			select {
			case <-ctx.Done():
				conn.Close()
				return
			case <-done:
				return
			case <-ticker.C:
				_ = conn.SetWriteDeadline(time.Now().Add(writeWait))
				if err := conn.WriteMessage(websocket.PingMessage, nil); err != nil {
					conn.Close()
					return
				}
			}
		}
	}()

	for {
		msgType, data, err := conn.ReadMessage()
		if err != nil {
			return err
		}
		_ = conn.SetReadDeadline(time.Now().Add(pongWait))

		if msgType != websocket.BinaryMessage {
			continue
		}
		e.handleMessage(data)
	}
}

func (e *Events) handleMessage(data []byte) {
	msg, err := DecodeUpdateMessage(data)
	if err != nil {
		logger.Debug("Undecodable Protect update", "error", err)
		return
	}

	if msg.Action.ModelKey != "camera" || msg.Action.Action != "update" {
		return
	}

	var patch CameraPatch
	if err := json.Unmarshal(msg.Payload, &patch); err != nil {
		logger.Debug("Unparsable camera patch", "error", err)
		return
	}

	if patch.State == nil && patch.IsMotionDetected == nil && patch.LastRing == nil && patch.LastMotion == nil {
		return
	}

	if e.OnCameraUpdate != nil {
		e.OnCameraUpdate(msg.Action.ID, patch)
	}
}

// sleepCtx waits d or until ctx is done; it returns false when ctx ended.
func sleepCtx(ctx context.Context, d time.Duration) bool {
	select {
	case <-ctx.Done():
		return false
	case <-time.After(d):
		return true
	}
}

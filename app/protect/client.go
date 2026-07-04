package protect

import (
	"bytes"
	"crypto/tls"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/cookiejar"
	"net/url"
	"strings"
	"sync"
	"time"

	"github.com/philipparndt/go-logger"
)

// Client talks to the UniFi Protect API on a UniFi OS console. Authentication
// uses a local user account (cookie + CSRF token), the same way the web UI
// does.
type Client struct {
	host       string
	username   string
	password   string
	verifySSL  bool
	httpClient *http.Client
	csrfToken  string
	mu         sync.RWMutex
}

func NewClient(host, username, password string, verifySSL bool) *Client {
	jar, _ := cookiejar.New(nil)

	transport := &http.Transport{
		TLSClientConfig: &tls.Config{
			InsecureSkipVerify: !verifySSL,
		},
	}

	return &Client{
		host:      strings.TrimSuffix(host, "/"),
		username:  username,
		password:  password,
		verifySSL: verifySSL,
		httpClient: &http.Client{
			Jar:       jar,
			Transport: transport,
			Timeout:   30 * time.Second,
		},
	}
}

func (c *Client) Host() string { return c.host }

func (c *Client) VerifySSL() bool { return c.verifySSL }

// Login authenticates against the UniFi OS console.
func (c *Client) Login() error {
	c.mu.Lock()
	defer c.mu.Unlock()

	// Step 1: fetch an initial CSRF token; some firmware versions hand it out
	// on any GET.
	if err := c.acquireCSRFToken(); err != nil {
		logger.Debug("No initial CSRF token", "error", err)
	}

	payload, err := json.Marshal(map[string]any{
		"username":   c.username,
		"password":   c.password,
		"token":      "",
		"rememberMe": true,
	})
	if err != nil {
		return err
	}

	req, err := http.NewRequest(http.MethodPost, c.host+"/api/auth/login", bytes.NewReader(payload))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")
	if c.csrfToken != "" {
		req.Header.Set("X-Csrf-Token", c.csrfToken)
	}

	resp, err := c.httpClient.Do(req)
	if err != nil {
		return fmt.Errorf("login request failed: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(resp.Body)
		return fmt.Errorf("login failed with status %d: %s", resp.StatusCode, string(body))
	}

	c.updateCSRFTokenLocked(resp.Header)
	logger.Info("Logged in to UniFi Protect", "host", c.host, "user", c.username)
	return nil
}

func (c *Client) acquireCSRFToken() error {
	req, err := http.NewRequest(http.MethodGet, c.host, nil)
	if err != nil {
		return err
	}
	resp, err := c.httpClient.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	_, _ = io.Copy(io.Discard, resp.Body)

	c.updateCSRFTokenLocked(resp.Header)
	return nil
}

// updateCSRFTokenLocked stores a rotated CSRF token from response headers.
// Callers must hold c.mu (write).
func (c *Client) updateCSRFTokenLocked(h http.Header) {
	for _, name := range []string{"X-Updated-Csrf-Token", "X-Csrf-Token"} {
		if token := h.Get(name); token != "" {
			c.csrfToken = token
			return
		}
	}
}

// BootstrapRaw returns the unparsed bootstrap JSON (diagnostics).
func (c *Client) BootstrapRaw() ([]byte, error) {
	return c.get(c.host + "/proxy/protect/api/bootstrap")
}

// Bootstrap fetches the full Protect state (NVR, cameras with channels) and
// the lastUpdateId needed to attach the realtime websocket.
func (c *Client) Bootstrap() (*Bootstrap, error) {
	data, err := c.BootstrapRaw()
	if err != nil {
		return nil, fmt.Errorf("bootstrap failed: %w", err)
	}

	var bs Bootstrap
	if err := json.Unmarshal(data, &bs); err != nil {
		return nil, fmt.Errorf("parsing bootstrap: %w", err)
	}
	if bs.NVR.Ports.RTSPS == 0 {
		bs.NVR.Ports.RTSPS = 7441
	}
	return &bs, nil
}

// Snapshot returns a JPEG snapshot of the camera. Width/height are hints; the
// NVR picks the closest available size (0 = NVR default).
func (c *Client) Snapshot(cameraID string, width, height int) ([]byte, error) {
	q := url.Values{}
	q.Set("ts", fmt.Sprintf("%d", time.Now().UnixMilli()))
	q.Set("force", "true")
	if width > 0 {
		q.Set("w", fmt.Sprintf("%d", width))
	}
	if height > 0 {
		q.Set("h", fmt.Sprintf("%d", height))
	}
	return c.get(c.host + "/proxy/protect/api/cameras/" + cameraID + "/snapshot?" + q.Encode())
}

// EnableRTSP switches on the RTSPS stream for the given channel ids of a
// camera. It PATCHes back the exact channel objects from the bootstrap with
// only isRtspEnabled changed, so no other channel settings are touched.
func (c *Client) EnableRTSP(cam *Camera, channelIDs []int) error {
	if len(cam.ChannelsRaw) == 0 {
		return fmt.Errorf("camera %s has no raw channel data", cam.Name)
	}

	var channels []map[string]any
	if err := json.Unmarshal(cam.ChannelsRaw, &channels); err != nil {
		return fmt.Errorf("parsing channels of %s: %w", cam.Name, err)
	}

	enable := map[int]bool{}
	for _, id := range channelIDs {
		enable[id] = true
	}
	for _, ch := range channels {
		// JSON numbers decode as float64.
		if id, ok := ch["id"].(float64); ok && enable[int(id)] {
			ch["isRtspEnabled"] = true
		}
	}

	payload, err := json.Marshal(map[string]any{"channels": channels})
	if err != nil {
		return err
	}

	_, err = c.patch(c.host+"/proxy/protect/api/cameras/"+cam.ID, payload)
	if err != nil {
		return fmt.Errorf("enabling RTSP on %s: %w", cam.Name, err)
	}
	logger.Info("Enabled RTSPS stream in Protect", "camera", cam.Name, "channels", channelIDs)
	return nil
}

// SetVideoCodec switches the camera's encoding (e.g. to "h264" — the Protect
// UI calls this "Standard" vs. "Enhanced"/H.265 encoding). The camera
// reconfigures its encoder in the background; streams reflect the new codec
// after a few seconds.
func (c *Client) SetVideoCodec(cam *Camera, codec string) error {
	payload, err := json.Marshal(map[string]any{"videoCodec": codec})
	if err != nil {
		return err
	}
	if _, err := c.patch(c.host+"/proxy/protect/api/cameras/"+cam.ID, payload); err != nil {
		return fmt.Errorf("setting video codec on %s: %w", cam.Name, err)
	}
	logger.Info("Switched camera video codec", "camera", cam.Name, "codec", codec)
	return nil
}

// RTSPSURL builds the stream URL for a channel alias, e.g.
// "rtsps://192.168.1.1:7441/abcDEF12?enableSrtp".
func (c *Client) RTSPSURL(bs *Bootstrap, alias string) string {
	host := c.host
	host = strings.TrimPrefix(host, "https://")
	host = strings.TrimPrefix(host, "http://")
	if i := strings.Index(host, ":"); i >= 0 {
		host = host[:i]
	}
	return fmt.Sprintf("rtsps://%s:%d/%s?enableSrtp", host, bs.NVR.Ports.RTSPS, alias)
}

// UpdatesWebsocketURL returns the realtime updates endpoint for the given
// bootstrap state.
func (c *Client) UpdatesWebsocketURL(lastUpdateID string) string {
	host := strings.TrimPrefix(strings.TrimPrefix(c.host, "https://"), "http://")
	return fmt.Sprintf("wss://%s/proxy/protect/ws/updates?lastUpdateId=%s", host, lastUpdateID)
}

// CookieHeader returns the session cookies as a single header value for the
// websocket handshake.
func (c *Client) CookieHeader() string {
	if c.httpClient.Jar == nil {
		return ""
	}
	u, err := url.Parse(c.host)
	if err != nil {
		return ""
	}
	var parts []string
	for _, cookie := range c.httpClient.Jar.Cookies(u) {
		parts = append(parts, cookie.Name+"="+cookie.Value)
	}
	return strings.Join(parts, "; ")
}

func (c *Client) get(url string) ([]byte, error) {
	req, err := http.NewRequest(http.MethodGet, url, nil)
	if err != nil {
		return nil, err
	}
	return c.doRequest(req)
}

func (c *Client) patch(url string, payload []byte) ([]byte, error) {
	req, err := http.NewRequest(http.MethodPatch, url, bytes.NewReader(payload))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Content-Type", "application/json")
	return c.doRequest(req)
}

func (c *Client) post(url string, payload []byte) ([]byte, error) {
	req, err := http.NewRequest(http.MethodPost, url, bytes.NewReader(payload))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Content-Type", "application/json")
	return c.doRequest(req)
}

// Reboot restarts a camera. Needed after a video codec switch: the camera
// encoder only picks up the new codec when its video pipeline restarts.
func (c *Client) Reboot(cam *Camera) error {
	if _, err := c.post(c.host+"/proxy/protect/api/cameras/"+cam.ID+"/reboot", []byte("{}")); err != nil {
		return fmt.Errorf("rebooting %s: %w", cam.Name, err)
	}
	logger.Info("Rebooting camera to apply new settings", "camera", cam.Name)
	return nil
}

// doRequest sends the request with the current CSRF token. On a 401 (session
// expired) it re-authenticates once and retries.
func (c *Client) doRequest(req *http.Request) ([]byte, error) {
	retryReq, cloneErr := cloneRequest(req)

	body, status, err := c.sendRequest(req)
	if err != nil {
		return nil, err
	}

	if status == http.StatusUnauthorized {
		if cloneErr != nil {
			return nil, fmt.Errorf("request failed with status %d: %s", status, string(body))
		}
		logger.Warn("Request unauthorized (401), re-authenticating", "url", req.URL.Path)
		if loginErr := c.Login(); loginErr != nil {
			return nil, fmt.Errorf("re-login after 401 failed: %w", loginErr)
		}
		body, status, err = c.sendRequest(retryReq)
		if err != nil {
			return nil, err
		}
	}

	if status < 200 || status >= 300 {
		return nil, fmt.Errorf("request failed with status %d: %s", status, string(body))
	}
	return body, nil
}

func cloneRequest(req *http.Request) (*http.Request, error) {
	clone := req.Clone(req.Context())
	if req.GetBody != nil {
		body, err := req.GetBody()
		if err != nil {
			return nil, err
		}
		clone.Body = body
	}
	return clone, nil
}

func (c *Client) sendRequest(req *http.Request) ([]byte, int, error) {
	c.mu.RLock()
	csrfToken := c.csrfToken
	c.mu.RUnlock()
	if csrfToken != "" {
		req.Header.Set("X-Csrf-Token", csrfToken)
	}

	resp, err := c.httpClient.Do(req)
	if err != nil {
		return nil, 0, err
	}
	defer resp.Body.Close()

	c.mu.Lock()
	c.updateCSRFTokenLocked(resp.Header)
	c.mu.Unlock()

	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, resp.StatusCode, err
	}
	return body, resp.StatusCode, nil
}

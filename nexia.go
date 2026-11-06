package main

// A client for the American Standard Home cloud (Nexia's platform), the API
// the phone app uses. There is no official API: this follows the open-source
// nexia library that Home Assistant uses (github.com/bdraco/nexia), and needs
// updating if American Standard changes theirs.

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"net"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"time"
)

const appVersion = "6.0.0"

var brandRoots = map[string]string{
	"asair": "https://asairhome.com",
	"trane": "https://www.tranehome.com",
	"nexia": "https://www.mynexia.com",
}

var errLoginBlocked = errors.New("not signing in again yet after a failed sign-in")

type nexiaClient struct {
	mobile string // root URL + "/mobile"
	acct   AccountConfig
	hc     *http.Client

	mu       sync.Mutex // one request at a time; guards the fields below
	mobileID string
	apiKey   string
	houseID  string
	etag     string
	devices  []any // the house's device list from the last full read
	// where the run-time history last answered (see runtimeHistory)
	historyBase string
	// A failed sign-in waits before the next, doubling each time: the cloud
	// locks the account after a few bad passwords
	loginWait  time.Duration
	loginAfter time.Time
}

func newNexiaClient(a AccountConfig) *nexiaClient {
	return &nexiaClient{
		mobile: brandRoots[a.Brand] + "/mobile",
		acct:   a,
		hc: &http.Client{
			Timeout: 20 * time.Second,
			// A redirect means the session expired; call handles it
			CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse },
		},
	}
}

// send makes one request with the app's headers and reads the whole reply.
func (c *nexiaClient) send(method, url string, body any, header http.Header) (*http.Response, []byte, error) {
	var rd io.Reader
	if body != nil {
		b, err := json.Marshal(body)
		if err != nil {
			return nil, nil, err
		}
		rd = bytes.NewReader(b)
	}
	req, err := http.NewRequest(method, url, rd)
	if err != nil {
		return nil, nil, err
	}
	req.Header.Set("Accept", "application/json")
	req.Header.Set("X-AppVersion", appVersion)
	req.Header.Set("X-AssociatedBrand", c.acct.Brand)
	if c.apiKey != "" {
		req.Header.Set("X-MobileId", c.mobileID)
		req.Header.Set("X-ApiKey", c.apiKey)
	}
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	for k, v := range header {
		req.Header[k] = v
	}
	resp, err := c.hc.Do(req)
	if err != nil {
		return nil, nil, err
	}
	defer resp.Body.Close()
	data, err := io.ReadAll(io.LimitReader(resp.Body, 16<<20))
	return resp, data, err
}

// call sends an authenticated request, signing in first if needed. A
// redirect or 401 means the session expired: it signs in again and retries
// once. A 304 returns a nil body.
func (c *nexiaClient) call(method, url string, body any, header http.Header) (map[string]any, http.Header, error) {
	for attempt := 0; ; attempt++ {
		if c.apiKey == "" {
			if err := c.login(); err != nil {
				return nil, nil, err
			}
		}
		resp, data, err := c.send(method, url, body, header)
		if err != nil {
			return nil, nil, err
		}
		switch {
		case (resp.StatusCode == http.StatusFound || resp.StatusCode == http.StatusUnauthorized) && attempt == 0:
			c.apiKey = ""
			continue
		case resp.StatusCode == http.StatusNotModified:
			return nil, resp.Header, nil
		case resp.StatusCode/100 != 2:
			return nil, nil, fmt.Errorf("%s %s: HTTP %d%s", method, strings.TrimPrefix(url, c.mobile), resp.StatusCode, snippet(data))
		}
		m, err := decodeJSON(data)
		if err != nil {
			return nil, nil, fmt.Errorf("%s %s: %w", method, strings.TrimPrefix(url, c.mobile), err)
		}
		return m, resp.Header, nil
	}
}

func (c *nexiaClient) login() error {
	if time.Now().Before(c.loginAfter) {
		return fmt.Errorf("%w (until %s)", errLoginBlocked, c.loginAfter.Format("15:04"))
	}
	c.mobileID, c.apiKey = "", ""
	resp, data, err := c.send(http.MethodPost, c.mobile+"/accounts/sign_in", map[string]any{
		"login":         c.acct.Username,
		"password":      c.acct.Password,
		"children":      []any{},
		"childSchemas":  []any{},
		"commitModel":   nil,
		"nextHref":      nil,
		"device_uuid":   c.acct.DeviceUUID,
		"device_name":   c.acct.DeviceName,
		"app_version":   appVersion,
		"is_commercial": false,
	}, nil)
	if err != nil {
		return fmt.Errorf("sign in: %w", err) // network trouble doesn't count against the account
	}
	m, _ := decodeJSON(data)
	result, _ := m["result"].(map[string]any)
	if resp.StatusCode/100 != 2 || m["success"] != true || str(result["api_key"]) == "" {
		reason := str(m["error"])
		if reason == "" {
			reason = fmt.Sprintf("HTTP %d", resp.StatusCode)
		}
		c.loginWait = min(max(2*c.loginWait, 15*time.Minute), 6*time.Hour)
		c.loginAfter = time.Now().Add(c.loginWait)
		return fmt.Errorf("sign in to American Standard Home failed: %s; next try at %s", reason, c.loginAfter.Format("15:04"))
	}
	c.mobileID, c.apiKey = str(result["mobile_id"]), str(result["api_key"])
	c.loginWait, c.loginAfter = 0, time.Time{}
	log.Printf("🔑 signed in to American Standard Home as %s", c.acct.Username)
	return nil
}

// house reads the account's device list. It returns changed=false when
// nothing changed since the last read (the cloud answers 304 to the ETag).
func (c *nexiaClient) house() (devices []any, changed bool, err error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.houseID == "" {
		m, _, err := c.call(http.MethodPost, c.mobile+"/session",
			map[string]any{"app_version": appVersion, "device_uuid": c.acct.DeviceUUID}, nil)
		if err != nil {
			return nil, false, err
		}
		data, _ := dig(m, "result", "_links", "child", 0, "data").(map[string]any)
		if c.houseID = str(data["id"]); c.houseID == "" {
			return nil, false, fmt.Errorf("the account has no home in American Standard Home")
		}
	}
	header := http.Header{}
	if c.etag != "" && c.devices != nil {
		header.Set("If-None-Match", c.etag)
	}
	m, h, err := c.call(http.MethodGet, c.mobile+"/houses/"+c.houseID, nil, header)
	if err != nil {
		return nil, false, err
	}
	if m == nil {
		return c.devices, false, nil
	}
	children, _ := dig(m, "result", "_links", "child").([]any)
	var devs any
	for _, ch := range children {
		ch, _ := ch.(map[string]any)
		if strings.Contains(str(ch["href"]), "/devices") {
			devs = ch["data"]
		}
	}
	if devs == nil && len(children) > 0 {
		devs = dig(children[0], "data")
	}
	if items, ok := dig(devs, "items").([]any); ok {
		devs = items
	}
	c.devices, _ = devs.([]any)
	c.etag = h.Get("Etag")
	return c.devices, true, nil
}

// Forget the ETag so the next read is a full one, after a change.
func (c *nexiaClient) invalidate() {
	c.mu.Lock()
	c.etag = ""
	c.mu.Unlock()
}

// post sends a change to an href the cloud gave us, or one we built.
func (c *nexiaClient) post(url, method string, payload any) error {
	if method != http.MethodPost && method != http.MethodPut {
		return fmt.Errorf("unsupported method %q for %s", method, url)
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	_, _, err := c.call(method, url, payload, nil)
	if err == nil {
		c.etag = ""
	}
	return err
}

// runtimeHistory reads the thermostat's own run-time totals: the data behind
// the website's Daily History chart, at
// /runtime_history/XxlThermostat-{id}/{daily|monthly}.json. The website
// serves it, and it's not known which hosts accept the app's key, so each is
// tried in turn and the one that answers is tried first next time. A
// redirect here means "not on this host", not an expired session: the poll
// signs in again when the session really expires.
func (c *nexiaClient) runtimeHistory(thermostatID, period string) (map[string]any, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.apiKey == "" {
		if err := c.login(); err != nil {
			return nil, err
		}
	}
	path := fmt.Sprintf("/runtime_history/XxlThermostat-%s/%s.json", url.PathEscape(thermostatID), period)
	var fails []string
	for _, base := range c.historyBases() {
		resp, data, err := c.send(http.MethodGet, base+path, nil, nil)
		if err != nil {
			fails = append(fails, fmt.Sprintf("%s: %v", base, err))
			continue
		}
		if resp.StatusCode/100 == 2 {
			if m, err := decodeJSON(data); err == nil && m["series"] != nil {
				c.historyBase = base
				return m, nil
			}
			fails = append(fails, fmt.Sprintf("%s: not history data%s", base, snippet(data)))
			continue
		}
		fails = append(fails, fmt.Sprintf("%s: HTTP %d", base, resp.StatusCode))
	}
	return nil, fmt.Errorf("could not read the run-time history (%s)", strings.Join(fails, "; "))
}

// historyBases lists where to look for the run-time history: where it last
// answered, the app API, the site root, and the www. host the website uses.
func (c *nexiaClient) historyBases() []string {
	root := strings.TrimSuffix(c.mobile, "/mobile")
	list := []string{c.historyBase, c.mobile, root}
	if u, err := url.Parse(root); err == nil && net.ParseIP(u.Hostname()) == nil && !strings.HasPrefix(u.Host, "www.") {
		u.Host = "www." + u.Host
		list = append(list, u.String())
	}
	var out []string
	seen := map[string]bool{"": true}
	for _, b := range list {
		if !seen[b] {
			seen[b] = true
			out = append(out, b)
		}
	}
	return out
}

// ---- Reading the JSON ----

func decodeJSON(data []byte) (map[string]any, error) {
	d := json.NewDecoder(bytes.NewReader(data))
	d.UseNumber() // ids are large integers
	var m map[string]any
	if err := d.Decode(&m); err != nil {
		return nil, fmt.Errorf("unexpected reply%s", snippet(data))
	}
	return m, nil
}

// dig walks maps by key and lists by index; nil when any step is missing.
func dig(v any, path ...any) any {
	for _, p := range path {
		switch k := p.(type) {
		case string:
			m, ok := v.(map[string]any)
			if !ok {
				return nil
			}
			v = m[k]
		case int:
			l, ok := v.([]any)
			if !ok || k >= len(l) {
				return nil
			}
			v = l[k]
		}
	}
	return v
}

// findBy returns the object in list whose key equals value, as the app's
// "features" (by name) and "settings" (by type) are looked up.
func findBy(list any, key, value string) map[string]any {
	l, _ := list.([]any)
	for _, x := range l {
		if m, ok := x.(map[string]any); ok && str(m[key]) == value {
			return m
		}
	}
	return nil
}

func str(v any) string {
	switch x := v.(type) {
	case nil:
		return ""
	case string:
		return x
	case json.Number:
		return x.String()
	}
	return fmt.Sprint(v)
}

// num reads a number the cloud may send as a number or a string; nil for
// "--" and other blanks.
func num(v any) *float64 {
	var f float64
	switch x := v.(type) {
	case json.Number:
		var err error
		if f, err = x.Float64(); err != nil {
			return nil
		}
	case float64:
		f = x
	case string:
		if _, err := fmt.Sscanf(strings.TrimSpace(x), "%g", &f); err != nil {
			return nil
		}
	default:
		return nil
	}
	return &f
}

func snippet(b []byte) string {
	s := strings.Join(strings.Fields(string(b)), " ")
	if s == "" {
		return ""
	}
	if len(s) > 160 {
		s = s[:160] + "…"
	}
	return ": " + s
}

// actionURL finds where the cloud wants a change sent: the item's "self"
// action, then the named one, as the app does. Empty when it names neither.
func actionURL(item map[string]any, name string) (url, method string) {
	for _, a := range []string{"self", name} {
		if act, ok := dig(item, "actions", a).(map[string]any); ok && str(act["href"]) != "" {
			method = strings.ToUpper(str(act["method"]))
			if method == "" {
				method = http.MethodPost
			}
			return str(act["href"]), method
		}
	}
	return "", ""
}

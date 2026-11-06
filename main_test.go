package main

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"
)

// A trimmed XL824 house in the shape the nexia library reads
const houseJSON = `{"success": true, "result": {"id": 123, "name": "Home", "_links": {"child": [
  {"href": "https://x/mobile/houses/123/devices", "data": {"items": [
    {"id": 456789, "name": "Downstairs", "type": "xxl_thermostat",
     "system_status": "Cooling", "indoor_humidity": "45", "outdoor_temperature": "101",
     "has_outdoor_temperature": true,
     "features": [
       {"name": "advanced_info", "items": [{"label": "Model", "value": "XL824"}, {"label": "Firmware Version", "value": "5.9.1"}]},
       {"name": "thermostat", "scale": "f", "setpoint_heat_min": 55, "setpoint_cool_max": 99, "setpoint_delta": 3},
       {"name": "runtime_history", "actions": {
         "get_runtime_history": {"href": "HISTORY_URL?report_type=daily", "method": "GET"},
         "get_monthly_runtime_history": {"href": "HISTORY_URL?report_type=monthly", "method": "GET"}}},
       {"name": "thermostat_fan_mode", "actions": {"update_thermostat_fan_mode": {"href": "FAN_URL", "method": "POST"}}}
     ],
     "settings": [
       {"type": "fan_mode", "title": "Fan Mode", "current_value": "auto",
        "options": [{"value": "auto", "label": "Auto"}, {"value": "on", "label": "On"}, {"value": "circulate", "label": "Circulate"}]},
       {"type": "emergency_heat", "title": "Emergency Heat", "current_value": false,
        "options": [{"value": true, "label": "On"}, {"value": false, "label": "Off"}]},
       {"type": "system_mode", "current_value": "COOL", "options": [{"value": "COOL", "label": "Cool"}]},
       {"type": "run_mode", "current_value": "run_schedule",
        "options": [{"value": "permanent_hold", "label": "Permanent Hold"}, {"value": "run_schedule", "label": "Run Schedule"}]},
       {"type": "fan_speed", "current_value": 0.35, "values": [0.35, 0.5, 0.75]}
     ],
     "zones": [
       {"id": 1, "name": "Native Zone", "temperature": 78, "setpoints": {"heat": 68, "cool": 76},
        "zone_status": "Cooling", "settings": [],
        "features": [
          {"name": "thermostat", "actions": {"set_setpoints": {"href": "SETPOINTS_URL"}}},
{"name": "thermostat_mode", "value": "COOL",
           "actions": {"update_thermostat_mode": {"href": "MODE_URL"}}},
          {"name": "thermostat_run_mode", "value": "run_schedule",
           "actions": {"update_thermostat_run_mode": {"href": "RUNMODE_URL"}}}
        ]}
     ]},
    {"id": 99, "name": "Front door", "type": "lock"}
  ]}}
]}}}`

// fakeCloud is enough of the American Standard Home cloud for the client.
type fakeCloud struct {
	mu        sync.Mutex
	logins    int
	badPass   bool
	expired   bool // the next authenticated call redirects to sign-in
	posts     []string
	houseGets int
}

func (f *fakeCloud) handler(t *testing.T) http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("POST /mobile/accounts/sign_in", func(w http.ResponseWriter, r *http.Request) {
		f.mu.Lock()
		defer f.mu.Unlock()
		f.logins++
		var body map[string]any
		json.NewDecoder(r.Body).Decode(&body)
		if r.Header.Get("X-AssociatedBrand") != "asair" || body["login"] != "me@example.com" {
			t.Errorf("sign-in brand %q login %v", r.Header.Get("X-AssociatedBrand"), body["login"])
		}
		if f.badPass {
			io.WriteString(w, `{"success": false, "error": "Invalid email or password"}`)
			return
		}
		io.WriteString(w, `{"success": true, "result": {"mobile_id": 987654321012, "api_key": "KEY"}}`)
	})
	auth := func(h http.HandlerFunc) http.HandlerFunc {
		return func(w http.ResponseWriter, r *http.Request) {
			f.mu.Lock()
			expired := f.expired
			f.expired = false
			f.mu.Unlock()
			if expired || r.Header.Get("X-ApiKey") != "KEY" || r.Header.Get("X-MobileId") != "987654321012" {
				http.Redirect(w, r, "/login", http.StatusFound)
				return
			}
			h(w, r)
		}
	}
	mux.HandleFunc("POST /mobile/session", auth(func(w http.ResponseWriter, r *http.Request) {
		io.WriteString(w, `{"success": true, "result": {"_links": {"child": [{"data": {"id": 123, "name": "Home"}}]}}}`)
	}))
	mux.HandleFunc("GET /mobile/houses/123", auth(func(w http.ResponseWriter, r *http.Request) {
		f.mu.Lock()
		f.houseGets++
		f.mu.Unlock()
		if r.Header.Get("If-None-Match") == `"v1"` {
			w.WriteHeader(http.StatusNotModified)
			return
		}
		w.Header().Set("ETag", `"v1"`)
		base := "http://" + r.Host + "/mobile"
		io.WriteString(w, strings.NewReplacer("SETPOINTS_URL", base+"/xxl_zones/1/setpoints", "MODE_URL", base+"/xxl_zones/1/zone_mode",
			"RUNMODE_URL", base+"/xxl_zones/1/run_mode", "FAN_URL", base+"/xxl_thermostats/456789/fan_mode",
			// The real cloud names the shared Nexia host, to test moving it onto ours
			"HISTORY_URL", "http://nexia.invalid/mobile/runtime_history/TraneXl824-456789").Replace(houseJSON))
	}))
	// Only daily: monthly fails, to test the error
	mux.HandleFunc("GET /mobile/runtime_history/{dev}", auth(func(w http.ResponseWriter, r *http.Request) {
		f.mu.Lock()
		f.posts = append(f.posts, r.Method+" "+r.URL.String())
		f.mu.Unlock()
		if r.PathValue("dev") != "TraneXl824-456789" || r.URL.Query().Get("report_type") != "daily" {
			http.NotFound(w, r)
			return
		}
		io.WriteString(w, `{"success": true, "result": {"categories": ["Thursday", "Friday"], "series": [
			{"name": "Stage 2 Cool", "data": [0, 0.8], "stack": "cool", "state": "cool_stage2"},
			{"name": "Cool", "data": [6.2, 1.1], "stack": "cool", "state": "cool_stage1"}]}}`)
	}))
	mux.HandleFunc("/", auth(func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		f.mu.Lock()
		f.posts = append(f.posts, r.Method+" "+r.URL.Path+" "+string(b))
		f.mu.Unlock()
		io.WriteString(w, `{"success": true, "result": {}}`)
	}))
	return mux
}

func testClient(t *testing.T) (*nexiaClient, *fakeCloud) {
	f := &fakeCloud{}
	srv := httptest.NewServer(f.handler(t))
	t.Cleanup(srv.Close)
	c := newNexiaClient(AccountConfig{Brand: "asair", Username: "me@example.com", Password: "pw", DeviceUUID: uuidFrom("me")})
	c.mobile = srv.URL + "/mobile"
	return c, f
}

func TestParseThermostat(t *testing.T) {
	m, err := decodeJSON([]byte(houseJSON))
	if err != nil {
		t.Fatal(err)
	}
	devs, _ := dig(m, "result", "_links", "child", 0, "data", "items").([]any)
	ts := parseThermostats(devs)
	if len(ts) != 1 {
		t.Fatalf("got %d thermostats, want 1 (the lock is skipped)", len(ts))
	}
	th := ts[0]
	if th.ID != "456789" || th.Model != "XL824" || th.Firmware != "5.9.1" || th.Unit != "F" {
		t.Errorf("thermostat %+v", th)
	}
	if th.RunState != "cooling" || *th.Humidity != 45 || *th.Outdoor != 101 || th.Deadband != 3 || th.CoolMax != 99 {
		t.Errorf("state %q humidity %v outdoor %v deadband %v", th.RunState, *th.Humidity, *th.Outdoor, th.Deadband)
	}
	var types []string
	for _, s := range th.Settings {
		types = append(types, s.Type)
	}
	if got := strings.Join(types, ","); got != "fan_mode,emergency_heat,fan_speed" {
		t.Errorf("settings %s; system_mode and run_mode have their own controls", got)
	}
	z := th.Zones[0]
	if z.ID != "456789_1" || *z.Temp != 78 || *z.Heat != 68 || *z.Cool != 76 || z.Mode != "COOL" {
		t.Errorf("zone %+v", z)
	}
	if !z.CanHold || z.Hold || z.RunMode != "Run Schedule" {
		t.Errorf("run mode: can %v hold %v %q", z.CanHold, z.Hold, z.RunMode)
	}
}

func TestPlan(t *testing.T) {
	m, _ := decodeJSON([]byte(houseJSON))
	devs, _ := dig(m, "result", "_links", "child", 0, "data", "items").([]any)
	th := parseThermostats(devs)[0]
	fl := func(f float64) *float64 { return &f }
	tests := []struct {
		c                    Command
		url, method, payload string
		noop                 bool
	}{
		{Command{Action: "mode", Value: "heat"}, "MODE_URL", "POST", `{"value":"HEAT"}`, false},
		{Command{Action: "mode", Value: "COOL"}, "MODE_URL", "POST", `{"value":"COOL"}`, true},
		// Lowering cool keeps heat the deadband below
		{Command{Action: "setpoints", Cool: fl(70.4)}, "SETPOINTS_URL", "POST", `{"cool":70,"heat":67}`, false},
		{Command{Action: "setpoints", Heat: fl(75)}, "SETPOINTS_URL", "POST", `{"cool":78,"heat":75}`, false},
		{Command{Action: "hold"}, "RUNMODE_URL", "POST", `{"value":"permanent_hold"}`, false},
		{Command{Action: "schedule"}, "RUNMODE_URL", "POST", `{"value":"run_schedule"}`, true},
		{Command{Action: "setting", Setting: "fan_mode", Value: "on"}, "FAN_URL", "POST", `{"value":"on"}`, false},
		{Command{Action: "setting", Setting: "emergency_heat", Value: "true"}, "M/xxl_thermostats/456789/emergency_heat", "POST", `{"value":"true"}`, false},
		{Command{Action: "setting", Setting: "fan_speed", Value: "0.5"}, "M/xxl_thermostats/456789/fan_speed", "POST", `{"value":0.5}`, false},
	}
	for _, tt := range tests {
		req, err := th.plan("M", tt.c)
		if err != nil {
			t.Errorf("%+v: %v", tt.c, err)
			continue
		}
		payload, _ := json.Marshal(req.payload)
		if req.url != tt.url || req.method != tt.method || string(payload) != tt.payload || req.noop != tt.noop {
			t.Errorf("%+v: got %s %s %s noop %v", tt.c, req.method, req.url, payload, req.noop)
		}
	}
	for _, c := range []Command{
		{Action: "mode", Value: "dry"},
		{Action: "setpoints"},
		{Action: "setpoints", Heat: fl(70), Cool: fl(71)}, // inside the deadband
		{Action: "setpoints", Cool: fl(105)},              // above the limit
		{Action: "setting", Setting: "fan_mode", Value: "turbo"},
		{Action: "setting", Setting: "nope", Value: "x"},
		{Action: "mode", Zone: "nope", Value: "COOL"},
	} {
		if _, err := th.plan("M", c); err == nil {
			t.Errorf("%+v: no error", c)
		}
	}
}

func TestClientSignInAndETag(t *testing.T) {
	c, f := testClient(t)
	devs, changed, err := c.house()
	if err != nil || !changed || len(parseThermostats(devs)) != 1 {
		t.Fatalf("first read: changed %v err %v", changed, err)
	}
	if _, changed, err = c.house(); err != nil || changed {
		t.Errorf("second read should be a 304: changed %v err %v", changed, err)
	}
	// An expired session is signed into again and the request retried
	f.expired = true
	if _, _, err = c.house(); err != nil {
		t.Fatal(err)
	}
	if f.logins != 2 {
		t.Errorf("signed in %d times, want 2", f.logins)
	}
	if err := c.post(strings.Replace(c.mobile, "/mobile", "", 1)+"/mobile/xxl_zones/1/zone_mode", "POST", map[string]any{"value": "HEAT"}); err != nil {
		t.Fatal(err)
	}
	if len(f.posts) != 1 || f.posts[0] != `POST /mobile/xxl_zones/1/zone_mode {"value":"HEAT"}` {
		t.Errorf("posts %q", f.posts)
	}
	if c.etag != "" {
		t.Error("a change should make the next read a full one")
	}
}

func TestRuntimeHistory(t *testing.T) {
	c, f := testClient(t)
	h := newHVAC(c, time.Minute)
	if _, err := h.RuntimeHistory("456789", "daily"); err != errNotReady {
		t.Errorf("before the first read: %v", err)
	}
	h.history = map[string]historyEntry{}
	h.refresh()
	m, err := h.RuntimeHistory("456789", "daily")
	if err != nil {
		t.Fatal(err)
	}
	// "result" is unwrapped
	if s, _ := m["series"].([]any); len(s) != 2 {
		t.Errorf("series %v", m["series"])
	}
	if f.logins != 1 {
		t.Errorf("signed in %d times, want 1", f.logins)
	}
	// A second read within 15 minutes comes from the cache
	f.mu.Lock()
	f.posts = nil
	f.mu.Unlock()
	if _, err := h.RuntimeHistory("456789", "daily"); err != nil || len(f.posts) != 0 {
		t.Errorf("cached read: err %v, requests %q", err, f.posts)
	}
	if _, err := h.RuntimeHistory("456789", "monthly"); err == nil {
		t.Error("monthly isn't served by the fake, want an error")
	}
}

func TestClientBadPasswordBacksOff(t *testing.T) {
	c, f := testClient(t)
	f.badPass = true
	if _, _, err := c.house(); err == nil || !strings.Contains(err.Error(), "Invalid email or password") {
		t.Fatalf("got %v", err)
	}
	// The next try waits instead of hammering the account into a lockout
	if _, _, err := c.house(); err == nil || !strings.Contains(err.Error(), "not signing in again") {
		t.Fatalf("got %v", err)
	}
	if f.logins != 1 {
		t.Errorf("signed in %d times, want 1", f.logins)
	}
}

func TestSendThroughHVAC(t *testing.T) {
	c, f := testClient(t)
	h := newHVAC(c, time.Minute)
	if err := h.Send("me", "456789", Command{Action: "mode", Value: "HEAT"}); err != errNotReady {
		t.Errorf("before the first read: %v", err)
	}
	h.refresh()
	if err := h.Send("me", "456789", Command{Action: "mode", Value: "HEAT"}); err != nil {
		t.Fatal(err)
	}
	// Same as now: nothing is sent
	if err := h.Send("me", "456789", Command{Action: "setting", Setting: "fan_mode", Value: "auto"}); err != nil {
		t.Fatal(err)
	}
	if len(f.posts) != 1 || f.posts[0] != `POST /mobile/xxl_zones/1/zone_mode {"value":"HEAT"}` {
		t.Errorf("posts %q", f.posts)
	}
	if s := h.Snapshot(); len(s.Thermostats) != 1 || s.Updated == nil || s.Error != "" {
		t.Errorf("snapshot %+v", s)
	}
}

func TestRuntimes(t *testing.T) {
	loc := time.FixedZone("AZ", -7*3600)
	day := time.Date(2026, 7, 1, 0, 0, 0, 0, loc)
	at := func(h, m int) time.Time { return day.Add(time.Duration(h)*time.Hour + time.Duration(m)*time.Minute) }
	rs := []Reading{
		{At: at(10, 0), RunState: "cooling"},
		{At: at(10, 2), RunState: "cooling"},
		{At: at(10, 4), RunState: "idle"},
		{At: at(12, 0), RunState: "cooling"}, // then the cloud drops out: counted for maxGap only
		{At: at(23, 59), RunState: "heating"},
		{At: at(24, 1), RunState: "idle"}, // 1 min each side of midnight
	}
	got := runtimes(rs, day, 2, 4*time.Minute)
	if got[0].Date != "2026-07-01" || got[0].Cooling != 8 || got[0].Heating != 1 || got[1].Heating != 1 {
		t.Errorf("got %+v", got)
	}
}

func TestScheduleValidate(t *testing.T) {
	m, _ := decodeJSON([]byte(houseJSON))
	devs, _ := dig(m, "result", "_links", "child", 0, "data", "items").([]any)
	th := parseThermostats(devs)[0]
	fl := func(f float64) *float64 { return &f }
	ok := Schedule{Name: "Pre-cool", Days: 62, At: "14:00", Mode: "cool", Cool: fl(72.4), Hold: "hold"}
	if err := ok.Validate(&th); err != nil {
		t.Fatal(err)
	}
	if ok.ZoneID != "456789_1" || ok.Mode != "COOL" || *ok.Cool != 72 {
		t.Errorf("normalised to %+v", ok)
	}
	if cmds := ok.commands(); len(cmds) != 3 || cmds[0].Action != "mode" || cmds[1].Action != "setpoints" || cmds[2].Action != "hold" {
		t.Errorf("commands %+v", cmds)
	}
	for _, bad := range []Schedule{
		{Days: 1, At: "14:00"},                            // changes nothing
		{Days: 0, At: "14:00", Mode: "COOL"},              // no day
		{Days: 1, At: "2pm", Mode: "COOL"},                // bad time
		{Days: 1, At: "14:00", Mode: "DRY"},               // bad mode
		{Days: 1, At: "14:00", Cool: fl(120)},             // above the limit
		{Days: 1, At: "14:00", Hold: "forever"},           // bad hold
		{Days: 1, At: "14:00", Mode: "COOL", ZoneID: "x"}, // unknown zone
	} {
		if err := bad.Validate(&th); err == nil {
			t.Errorf("%+v: no error", bad)
		}
	}
	due := Schedule{Enabled: true, Days: 1 << int(time.Wednesday), At: "14:00"}
	wed := time.Date(2026, 7, 1, 14, 0, 30, 0, time.Local)
	if !due.Due(wed) || due.Due(wed.Add(time.Minute)) || due.Due(wed.AddDate(0, 0, 1)) {
		t.Error("Due")
	}
	ran := wed.Add(-10 * time.Second)
	due.LastRun = &ran
	if due.Due(wed) {
		t.Error("ran this minute already")
	}
}

func TestRoles(t *testing.T) {
	cfg = &Config{}
	h := requireRole(func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(200) }, "admin", "user")
	for role, want := range map[string]int{"admin": 200, "user": 200, "guest": 403} {
		r := httptest.NewRequest("GET", "/", nil)
		r.Header.Set("X-Hub-Authenticated", "true")
		r.Header.Set("X-Hub-Role", role)
		w := httptest.NewRecorder()
		h(w, r)
		if w.Code != want {
			t.Errorf("%s: got %d want %d", role, w.Code, want)
		}
	}
	w := httptest.NewRecorder()
	h(w, httptest.NewRequest("GET", "/", nil))
	if w.Code != 401 {
		t.Errorf("without the hub: %d", w.Code)
	}
}

func TestUUIDStable(t *testing.T) {
	a, b := uuidFrom("Me@Example.com"), uuidFrom("me@example.com")
	if a != b || len(a) != 36 || a[14] != '4' {
		t.Errorf("%s %s", a, b)
	}
}

func TestAPI(t *testing.T) {
	c, f := testClient(t)
	cfg = &Config{PollSeconds: 120}
	cfg.Server.AllowDirect = true
	thermo = newHVAC(c, time.Minute)
	thermo.refresh()
	mux := newMux()
	do := func(method, path, body string) (int, string) {
		w := httptest.NewRecorder()
		mux.ServeHTTP(w, httptest.NewRequest(method, path, strings.NewReader(body)))
		return w.Code, w.Body.String()
	}
	if code, body := do("GET", "/api/thermostats", ""); code != 200 || !strings.Contains(body, `"model":"XL824"`) || !strings.Contains(body, `"database":false`) {
		t.Errorf("thermostats: %d %s", code, body)
	}
	if code, body := do("POST", "/api/thermostats/456789/command", `{"action":"setpoints","cool":74}`); code != 200 {
		t.Errorf("command: %d %s", code, body)
	}
	if len(f.posts) != 1 || f.posts[0] != `POST /mobile/xxl_zones/1/setpoints {"cool":74,"heat":68}` {
		t.Errorf("posts %q", f.posts)
	}
	if code, _ := do("POST", "/api/thermostats/456789/command", `{"action":"mode","value":"DRY"}`); code != 400 {
		t.Errorf("bad mode: %d", code)
	}
	if code, _ := do("GET", "/api/thermostats/456789/history", ""); code != 503 {
		t.Errorf("history without a database: %d", code)
	}
	if code, _ := do("GET", "/api/thermostats/nope/commands", ""); code != 404 {
		t.Errorf("unknown thermostat: %d", code)
	}
}

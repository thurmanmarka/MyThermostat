package main

import (
	"fmt"
	"math"
	"net/http"
	"slices"
	"strings"
)

// Thermostat is one thermostat as the page sees it.
type Thermostat struct {
	ID       string    `json:"id"`
	Name     string    `json:"name"`
	Model    string    `json:"model"`
	Firmware string    `json:"firmware"`
	Unit     string    `json:"unit"` // "F" or "C"
	Online   bool      `json:"online"`
	Status   string    `json:"status"`             // the thermostat's words, e.g. "Cooling", "System Idle"
	RunState string    `json:"run_state"`          // heating, cooling, idle, off or offline
	Humidity *float64  `json:"humidity,omitempty"` // indoor, %
	Outdoor  *float64  `json:"outdoor,omitempty"`
	HeatMin  float64   `json:"heat_min"`
	CoolMax  float64   `json:"cool_max"`
	Deadband float64   `json:"deadband"` // how far apart the heat and cool setpoints must be
	Settings []Setting `json:"settings"`
	Zones    []Zone    `json:"zones"`

	raw map[string]any
}

// Zone is a thermostat zone; a system without zoning has one.
type Zone struct {
	ID       string    `json:"id"`
	Name     string    `json:"name"`
	Temp     *float64  `json:"temp,omitempty"`
	Heat     *float64  `json:"heat,omitempty"` // setpoints
	Cool     *float64  `json:"cool,omitempty"`
	Mode     string    `json:"mode"` // AUTO, COOL, HEAT or OFF
	Modes    []string  `json:"modes"`
	Status   string    `json:"status"`
	CanHold  bool      `json:"can_hold"`
	Hold     bool      `json:"hold"`     // a permanent hold rather than the thermostat's schedule
	RunMode  string    `json:"run_mode"` // e.g. "Permanent Hold", "Run Schedule"
	Settings []Setting `json:"settings"`

	rawID string
	raw   map[string]any
}

// Setting is anything with a list of choices: fan mode, air cleaner,
// emergency heat, presets and whatever else the thermostat offers.
type Setting struct {
	Type    string   `json:"type"`
	Title   string   `json:"title"`
	Value   string   `json:"value"`
	Options []Option `json:"options"`

	raw map[string]any
}

type Option struct {
	Value string `json:"value"`
	Label string `json:"label"`

	raw any
}

var allModes = []string{"AUTO", "COOL", "HEAT", "OFF"}

// Settings with their own controls on the page, kept out of the list of others
var ownControl = map[string]bool{"system_mode": true, "zone_mode": true, "run_mode": true, "thermostat_run_mode": true}

// parseThermostats picks the thermostats out of the house's device list.
func parseThermostats(devices []any) []Thermostat {
	var out []Thermostat
	var add func(d map[string]any)
	add = func(d map[string]any) {
		typ := str(d["type"])
		if typ == "group" {
			subs, _ := dig(d, "_links", "child").([]any)
			for _, s := range subs {
				if m, ok := dig(s, "data").(map[string]any); ok {
					add(m)
				}
			}
			return
		}
		if typ != "" && !strings.Contains(typ, "thermostat") {
			return
		}
		// One without zones (an XL624) can't be controlled through this API
		if zs, _ := d["zones"].([]any); len(zs) > 0 {
			out = append(out, parseThermostat(d))
		}
	}
	for _, d := range devices {
		if m, ok := d.(map[string]any); ok {
			add(m)
		}
	}
	return out
}

func parseThermostat(d map[string]any) Thermostat {
	t := Thermostat{ID: str(d["id"]), Name: str(d["name"]), raw: d}
	info := findBy(d["features"], "name", "advanced_info")
	infoValue := func(labels ...string) string {
		for _, l := range labels {
			if it := findBy(info["items"], "label", l); it != nil {
				return str(it["value"])
			}
		}
		return ""
	}
	t.Model = infoValue("Model")
	t.Firmware = infoValue("Firmware Version", "Main Firmware Version")

	feat := findBy(d["features"], "name", "thermostat")
	t.Unit = strings.ToUpper(str(feat["scale"]))
	if t.Unit == "" {
		t.Unit = "F"
	}
	t.HeatMin, t.CoolMax, t.Deadband = fnum(feat["setpoint_heat_min"], 55), fnum(feat["setpoint_cool_max"], 90), fnum(feat["setpoint_delta"], 3)

	t.Status = str(d["system_status"])
	if t.Status == "" {
		t.Status = str(d["operating_state"])
	}
	if t.Status == "" {
		t.Status = str(feat["status"])
	}
	t.Online = t.Status != "NOT CONNECTED"
	t.RunState = runState(t.Status, t.Online)
	t.Humidity = num(d["indoor_humidity"])
	if t.Humidity != nil && *t.Humidity <= 1 {
		*t.Humidity *= 100 // some send a fraction
	}
	if d["has_outdoor_temperature"] != false {
		t.Outdoor = num(d["outdoor_temperature"])
	}
	t.Settings = parseSettings(d["settings"])

	zones, _ := d["zones"].([]any)
	for _, z := range zones {
		if m, ok := z.(map[string]any); ok {
			t.Zones = append(t.Zones, parseZone(&t, m))
		}
	}
	return t
}

func parseZone(t *Thermostat, z map[string]any) Zone {
	zn := Zone{rawID: str(z["id"]), Name: str(z["name"]), raw: z}
	zn.ID = zn.rawID
	if id := num(z["id"]); id != nil && *id < 256 {
		zn.ID = t.ID + "_" + zn.rawID // small ids are only unique within the thermostat
	}
	zn.Temp = num(z["temperature"])
	zn.Heat, zn.Cool = num(dig(z, "setpoints", "heat")), num(dig(z, "setpoints", "cool"))
	zn.Status = str(z["zone_status"])
	if zn.Status == "" {
		zn.Status = "Idle"
	}

	modeFeat := findBy(z["features"], "name", "thermostat_mode")
	zn.Mode = strings.ToUpper(str(modeFeat["value"]))
	if zn.Mode == "" {
		if s := zn.setting(t, "zone_mode"); s != nil {
			zn.Mode = strings.ToUpper(str(s["current_value"]))
		}
	}
	for _, o := range parseOptions(modeFeat) {
		if v := strings.ToUpper(o.Value); slices.Contains(allModes, v) {
			zn.Modes = append(zn.Modes, v)
		}
	}
	if len(zn.Modes) == 0 {
		zn.Modes = allModes
	}

	if rm := zn.runMode(t); rm != nil {
		zn.CanHold = true
		cur := str(rm["current_value"])
		zn.Hold = cur == "permanent_hold" || cur == "hold"
		zn.RunMode = humanize(cur)
		for _, o := range parseOptions(rm) {
			if o.Value == cur {
				zn.RunMode = o.Label
			}
		}
	}
	zn.Settings = parseSettings(z["settings"])
	return zn
}

// zoning: a zone has its own settings only when the system is zoned;
// otherwise they are the thermostat's, with zone_mode called system_mode.
func (z *Zone) setting(t *Thermostat, typ string) map[string]any {
	if l, _ := z.raw["settings"].([]any); len(l) > 0 {
		return findBy(l, "type", typ)
	}
	if typ == "zone_mode" {
		typ = "system_mode"
	}
	return findBy(t.raw["settings"], "type", typ)
}

// runMode is whether the zone follows its schedule or holds; nil when
// scheduling is off.
func (z *Zone) runMode(t *Thermostat) map[string]any {
	for _, key := range []string{"run_mode", "thermostat_run_mode"} {
		if s := z.setting(t, key); s != nil {
			return s
		}
		if f := findBy(z.raw["features"], "name", key); f != nil {
			cp := map[string]any{}
			for k, v := range f {
				cp[k] = v
			}
			cp["current_value"] = f["value"]
			return cp
		}
	}
	return nil
}

func parseSettings(list any) []Setting {
	out := []Setting{}
	l, _ := list.([]any)
	for _, x := range l {
		m, ok := x.(map[string]any)
		if !ok || ownControl[str(m["type"])] {
			continue
		}
		s := Setting{Type: str(m["type"]), Title: str(m["title"]), Value: str(m["current_value"]), Options: parseOptions(m), raw: m}
		if s.Title == "" {
			s.Title = str(m["label"])
		}
		if s.Title == "" {
			s.Title = humanize(s.Type)
		}
		if len(s.Options) >= 2 {
			out = append(out, s)
		}
	}
	return out
}

// parseOptions reads a choice list given either as options [{value, label}]
// or as parallel values and labels lists.
func parseOptions(m map[string]any) []Option {
	var out []Option
	if opts, ok := m["options"].([]any); ok {
		for _, o := range opts {
			om, _ := o.(map[string]any)
			if om == nil {
				continue
			}
			op := Option{Value: str(om["value"]), Label: str(om["label"]), raw: om["value"]}
			if op.Label == "" {
				op.Label = humanize(op.Value)
			}
			out = append(out, op)
		}
		return out
	}
	values, _ := m["values"].([]any)
	labels, _ := m["labels"].([]any)
	for i, v := range values {
		op := Option{Value: str(v), Label: humanize(str(v)), raw: v}
		if i < len(labels) {
			op.Label = str(labels[i])
		}
		out = append(out, op)
	}
	return out
}

func runState(status string, online bool) string {
	s := strings.ToLower(status)
	switch {
	case !online:
		return "offline"
	case strings.Contains(s, "cool"):
		return "cooling"
	case strings.Contains(s, "heat"):
		return "heating"
	case strings.Contains(s, "off"):
		return "off"
	}
	return "idle"
}

func fnum(v any, def float64) float64 {
	if f := num(v); f != nil {
		return *f
	}
	return def
}

func humanize(s string) string {
	s = strings.ReplaceAll(s, "_", " ")
	if s == "" {
		return s
	}
	return strings.ToUpper(s[:1]) + s[1:]
}

func (t *Thermostat) zone(id string) *Zone {
	for i := range t.Zones {
		if t.Zones[i].ID == id {
			return &t.Zones[i]
		}
	}
	if id == "" && len(t.Zones) > 0 {
		return &t.Zones[0]
	}
	return nil
}

func (t *Thermostat) roundTemp(f float64) float64 {
	if t.Unit == "C" {
		return math.Round(f*2) / 2
	}
	return math.Round(f)
}

// ---- Changes ----

// Command is one change to a thermostat.
type Command struct {
	Action  string   `json:"action"` // mode, setpoints, hold, schedule or setting
	Zone    string   `json:"zone"`   // zone id; empty = the first zone
	Value   string   `json:"value"`  // the mode, or the setting's option value
	Setting string   `json:"setting"`
	Heat    *float64 `json:"heat"`
	Cool    *float64 `json:"cool"`
}

// request is a change worked out against the thermostat's current state.
type request struct {
	url, method string
	payload     any
	detail      string // for the command log
	noop        bool   // already so; the cloud times out on a change to the same value
}

// plan works out where and what to send for c, checking it first.
func (t *Thermostat) plan(mobile string, c Command) (request, error) {
	z := t.zone(c.Zone)
	if z == nil {
		return request{}, fmt.Errorf("unknown zone %q", c.Zone)
	}
	zoneURL := func(end string) string { return mobile + "/xxl_zones/" + z.rawID + "/" + end }
	zoneFeature := func(name, action, fallback string) (string, string) {
		if url, m := actionURL(findBy(z.raw["features"], "name", name), action); url != "" {
			return url, m
		}
		return zoneURL(fallback), http.MethodPost
	}
	prefix := ""
	if len(t.Zones) > 1 {
		prefix = z.Name + ": "
	}

	switch c.Action {
	case "mode":
		mode := strings.ToUpper(c.Value)
		if !slices.Contains(z.Modes, mode) {
			return request{}, fmt.Errorf("mode must be one of %s", strings.Join(z.Modes, ", "))
		}
		url, m := zoneFeature("thermostat_mode", "update_thermostat_mode", "zone_mode")
		return request{url: url, method: m, payload: map[string]any{"value": mode},
			detail: prefix + "Mode: " + humanize(strings.ToLower(mode)), noop: mode == z.Mode}, nil

	case "setpoints":
		heat, cool, err := t.setpoints(z, c.Heat, c.Cool)
		if err != nil {
			return request{}, err
		}
		url, m := zoneFeature("thermostat", "set_setpoints", "setpoints")
		same := z.Heat != nil && z.Cool != nil && *z.Heat == heat && *z.Cool == cool
		return request{url: url, method: m, payload: map[string]any{"heat": heat, "cool": cool},
			detail: fmt.Sprintf("%sSetpoints: heat %g°, cool %g°", prefix, heat, cool), noop: same}, nil

	case "hold", "schedule":
		rm := z.runMode(t)
		want := []string{"permanent_hold", "hold"}
		detail := "Hold"
		if c.Action == "schedule" {
			want, detail = []string{"run_schedule", "schedule"}, "Follow schedule"
		}
		if rm == nil {
			if c.Action == "hold" {
				return request{}, fmt.Errorf("this thermostat has scheduling turned off, so it is always holding")
			}
			return request{url: zoneURL("return_to_schedule"), method: http.MethodPost, payload: map[string]any{},
				detail: prefix + detail}, nil
		}
		value := want[0]
		for _, o := range parseOptions(rm) {
			if slices.Contains(want, o.Value) {
				value = o.Value
				break
			}
		}
		url, m := zoneFeature("thermostat_run_mode", "update_thermostat_run_mode", "run_mode")
		return request{url: url, method: m, payload: map[string]any{"value": value},
			detail: prefix + detail, noop: slices.Contains(want, str(rm["current_value"]))}, nil

	case "setting":
		s, zoned := z.findSetting(t, c.Setting)
		if s == nil {
			return request{}, fmt.Errorf("unknown setting %q", c.Setting)
		}
		var opt *Option
		for i := range s.Options {
			if s.Options[i].Value == c.Value {
				opt = &s.Options[i]
			}
		}
		if opt == nil {
			return request{}, fmt.Errorf("%s can't be set to %q", s.Title, c.Value)
		}
		val := opt.raw
		if b, ok := val.(bool); ok {
			val = fmt.Sprint(b) // the cloud takes "true"/"false"
		}
		url, m := actionURL(s.raw, s.Type)
		if url == "" && s.Type == "fan_mode" {
			url, m = actionURL(findBy(t.raw["features"], "name", "thermostat_fan_mode"), "update_thermostat_fan_mode")
		}
		if url == "" {
			m = http.MethodPost
			if zoned || s.Type == "preset_selected" {
				url = zoneURL(s.Type)
			} else {
				url = mobile + "/xxl_thermostats/" + t.ID + "/" + s.Type
			}
		}
		return request{url: url, method: m, payload: map[string]any{"value": val},
			detail: fmt.Sprintf("%s%s: %s", prefix, s.Title, opt.Label), noop: c.Value == s.Value}, nil
	}
	return request{}, fmt.Errorf("unknown action %q", c.Action)
}

// findSetting looks in the zone's settings, then the thermostat's.
func (z *Zone) findSetting(t *Thermostat, typ string) (*Setting, bool) {
	for i := range z.Settings {
		if z.Settings[i].Type == typ {
			return &z.Settings[i], true
		}
	}
	for i := range t.Settings {
		if t.Settings[i].Type == typ {
			return &t.Settings[i], false
		}
	}
	return nil, false
}

// setpoints works out both setpoints from the ones given, keeping the other
// one at least the deadband away, as the app does.
func (t *Thermostat) setpoints(z *Zone, heatIn, coolIn *float64) (float64, float64, error) {
	if heatIn == nil && coolIn == nil {
		return 0, 0, fmt.Errorf("give a heat or cool setpoint")
	}
	curHeat, curCool := t.HeatMin, t.CoolMax
	if z.Heat != nil {
		curHeat = *z.Heat
	}
	if z.Cool != nil {
		curCool = *z.Cool
	}
	var heat, cool float64
	switch {
	case heatIn != nil && coolIn != nil:
		heat, cool = t.roundTemp(*heatIn), t.roundTemp(*coolIn)
	case heatIn != nil:
		heat = t.roundTemp(*heatIn)
		cool = max(curCool, heat+t.Deadband)
	default:
		cool = t.roundTemp(*coolIn)
		heat = min(curHeat, cool-t.Deadband)
	}
	switch {
	case heat >= cool:
		return 0, 0, fmt.Errorf("the heat setpoint must be below the cool setpoint")
	case cool-heat < t.Deadband:
		return 0, 0, fmt.Errorf("the heat and cool setpoints must be at least %g° apart", t.Deadband)
	case heat < t.HeatMin || cool > t.CoolMax:
		return 0, 0, fmt.Errorf("setpoints must be between %g° and %g°", t.HeatMin, t.CoolMax)
	}
	return heat, cool, nil
}

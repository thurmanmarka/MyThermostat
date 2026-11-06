package main

// Outdoor temperatures from the WXStation, through MyWeatherDash's API on
// the same Pi. The thermostat has no outdoor sensor, so the run-time
// table's Outdoor High/Low rows are empty without this.

import (
	"encoding/json"
	"fmt"
	"io"
	"math"
	"net/http"
	"strings"
	"sync"
	"time"
)

type outdoorSource struct {
	url string // MyWeatherDash, e.g. http://127.0.0.1:8081
	hc  *http.Client

	mu   sync.Mutex
	done map[string]*hiLo // finished days and months never change: "2026-10" / "2026-10-31"
}

type hiLo struct {
	Hi, Lo float64
}

func newOutdoorSource(url string) *outdoorSource {
	return &outdoorSource{url: url, hc: &http.Client{Timeout: 30 * time.Second}, done: map[string]*hiLo{}}
}

// periods returns the start of each of n days or months ending with the
// current one, plus where the last ends.
func periods(period string, n int, now time.Time) ([]time.Time, time.Time) {
	starts := make([]time.Time, n)
	for i := range starts {
		back := n - 1 - i
		if period == "monthly" {
			starts[i] = time.Date(now.Year(), now.Month()-time.Month(back), 1, 0, 0, 0, 0, now.Location())
		} else {
			starts[i] = time.Date(now.Year(), now.Month(), now.Day()-back, 0, 0, 0, 0, now.Location())
		}
	}
	end := time.Date(now.Year(), now.Month(), now.Day()+1, 0, 0, 0, 0, now.Location())
	if period == "monthly" {
		end = time.Date(now.Year(), now.Month()+1, 1, 0, 0, 0, 0, now.Location())
	}
	return starts, end
}

// highsLows is the outdoor high and low for each of n days or months ending
// now; nil where the station has no readings.
func (o *outdoorSource) highsLows(period string, n int, now time.Time) ([]*hiLo, error) {
	starts, end := periods(period, n, now)
	keyOf := func(t time.Time) string {
		if period == "monthly" {
			return t.Format("2006-01")
		}
		return t.Format("2006-01-02")
	}
	out := make([]*hiLo, n)
	o.mu.Lock()
	defer o.mu.Unlock()
	// Read from the first period not already known, to the end
	first := -1
	for i, s := range starts {
		if v, ok := o.done[keyOf(s)]; ok && i < n-1 {
			out[i] = v
			continue
		}
		first = i
		break
	}
	if first < 0 {
		return out, nil
	}
	rs, err := o.readings(starts[first], end)
	if err != nil {
		return nil, err
	}
	for _, r := range rs {
		// MyWeatherDash sends 0 for a missing reading
		if r.Temperature == 0 && r.Dewpoint == 0 {
			continue
		}
		i := first
		for i+1 < n && !r.Timestamp.Before(starts[i+1]) {
			i++
		}
		if r.Timestamp.Before(starts[i]) || !r.Timestamp.Before(end) {
			continue
		}
		if out[i] == nil {
			out[i] = &hiLo{Hi: r.Temperature, Lo: r.Temperature}
		}
		out[i].Hi = math.Max(out[i].Hi, r.Temperature)
		out[i].Lo = math.Min(out[i].Lo, r.Temperature)
	}
	// All but the current one are finished, including ones with no readings
	for i := first; i < n-1; i++ {
		o.done[keyOf(starts[i])] = out[i]
	}
	return out, nil
}

type wxReading struct {
	Timestamp   time.Time `json:"timestamp"`
	Temperature float64   `json:"temperature"`
	Dewpoint    float64   `json:"dewpoint"`
}

func (o *outdoorSource) readings(from, to time.Time) ([]wxReading, error) {
	url := fmt.Sprintf("%s/api/weather?start=%d&end=%d", o.url, from.Unix(), to.Unix())
	resp, err := o.hc.Get(url)
	if err != nil {
		return nil, fmt.Errorf("outdoor temperatures from MyWeatherDash: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		b, _ := io.ReadAll(io.LimitReader(resp.Body, 200))
		return nil, fmt.Errorf("outdoor temperatures from MyWeatherDash: HTTP %d %s", resp.StatusCode, strings.TrimSpace(string(b)))
	}
	var rs []wxReading
	if err := json.NewDecoder(resp.Body).Decode(&rs); err != nil {
		return nil, fmt.Errorf("outdoor temperatures from MyWeatherDash: %w", err)
	}
	return rs, nil
}

// fillOutdoor puts the station's highs and lows into the run-time
// history's table, in its Outdoor High/Low rows (added if missing). The
// values line up with the table's columns, which end with the current day
// or month.
func fillOutdoor(m map[string]any, his []*hiLo) {
	table, _ := m["table"].(map[string]any)
	if table == nil {
		return
	}
	series, _ := table["series"].([]any)
	row := func(pick func(hiLo) float64) []any {
		vals := make([]any, len(his))
		for i, h := range his {
			if h == nil {
				vals[i] = "-"
			} else {
				vals[i] = math.Round(pick(*h))
			}
		}
		return vals
	}
	set := func(word, name string, vals []any) {
		for _, s := range series {
			if s, ok := s.(map[string]any); ok && strings.Contains(strings.ToLower(str(s["name"])), "outdoor "+word) {
				s["data"] = vals
				return
			}
		}
		series = append(series, map[string]any{"name": name, "data": vals})
	}
	set("high", "Outdoor High Temp.", row(func(h hiLo) float64 { return h.Hi }))
	set("low", "Outdoor Low Temp.", row(func(h hiLo) float64 { return h.Lo }))
	table["series"] = series
	m["outdoor_source"] = "WXStation"
}

// columns is how many days or months the history covers.
func columns(m map[string]any) int {
	if h, ok := dig(m, "table", "headings").([]any); ok {
		return len(h)
	}
	if c, ok := dig(m, "chart", "xAxis", "categories").([]any); ok {
		return len(c)
	}
	return 0
}

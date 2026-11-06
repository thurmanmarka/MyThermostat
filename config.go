package main

import (
	"crypto/sha256"
	"fmt"
	"os"
	"strings"

	"gopkg.in/yaml.v3"
)

type Config struct {
	Server struct {
		Port int    `yaml:"port"`
		Bind string `yaml:"bind"`
		// AllowDirect lets requests that did not come through the hub act as admin.
		// Only for local testing; leave false in production.
		AllowDirect bool `yaml:"allow_direct"`
	} `yaml:"server"`
	Account  AccountConfig  `yaml:"account"`
	Database DatabaseConfig `yaml:"database"`
	// PollSeconds is how often the thermostats are read from the cloud. The
	// app's own library uses 120; much faster risks the account being throttled.
	PollSeconds int `yaml:"poll_seconds"`
	// Outdoor fills the run-time table's outdoor high/low from MyWeatherDash
	// (the WXStation), since the thermostat has no outdoor sensor.
	Outdoor struct {
		URL      string `yaml:"url"` // default http://127.0.0.1:8081
		Disabled bool   `yaml:"disabled"`
	} `yaml:"outdoor"`
}

// AccountConfig is the American Standard Home login the service signs in with.
type AccountConfig struct {
	// Brand picks the cloud: asair (American Standard), trane or nexia
	Brand    string `yaml:"brand"`
	Username string `yaml:"username"`
	Password string `yaml:"password"`
	// DeviceUUID names this service to the cloud, like a phone running the
	// app. Left empty, a fixed one is made from the username.
	DeviceUUID string `yaml:"device_uuid"`
	DeviceName string `yaml:"device_name"`
}

// DatabaseConfig is where readings, the command log and schedules are kept.
// When disabled, the thermostat can still be controlled but nothing is
// remembered and schedules are off.
type DatabaseConfig struct {
	Enabled       bool   `yaml:"enabled"`
	Host          string `yaml:"host"`
	Port          int    `yaml:"port"`
	Name          string `yaml:"name"`
	User          string `yaml:"user"`
	Password      string `yaml:"password"`
	RetentionDays int    `yaml:"retention_days"`
}

func LoadConfig(path string) (*Config, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	var c Config
	if err := yaml.Unmarshal(data, &c); err != nil {
		return nil, err
	}
	if c.Server.Port == 0 {
		c.Server.Port = 8086
	}
	if c.Server.Bind == "" {
		c.Server.Bind = "127.0.0.1"
	}
	if c.Outdoor.URL == "" {
		c.Outdoor.URL = "http://127.0.0.1:8081"
	}
	c.Outdoor.URL = strings.TrimSuffix(c.Outdoor.URL, "/")
	if c.PollSeconds == 0 {
		c.PollSeconds = 120
	}
	if c.PollSeconds < 30 {
		return nil, fmt.Errorf("poll_seconds must be at least 30")
	}
	a := &c.Account
	if a.Brand == "" {
		a.Brand = "asair"
	}
	if _, ok := brandRoots[a.Brand]; !ok {
		return nil, fmt.Errorf("account.brand must be asair, trane or nexia, not %q", a.Brand)
	}
	if a.Username == "" || a.Password == "" {
		return nil, fmt.Errorf("account.username and account.password are required")
	}
	if a.DeviceUUID == "" {
		a.DeviceUUID = uuidFrom(a.Username)
	}
	if a.DeviceName == "" {
		a.DeviceName = "MyThermostat"
	}
	if c.Database.RetentionDays == 0 {
		c.Database.RetentionDays = 730
	}
	return &c, nil
}

// uuidFrom makes a version 4 style UUID that stays the same for a username,
// so restarts don't look like a new phone signing in each time.
func uuidFrom(s string) string {
	h := sha256.Sum256([]byte("MyThermostat:" + strings.ToLower(s)))
	h[6] = h[6]&0x0f | 0x40
	h[8] = h[8]&0x3f | 0x80
	return fmt.Sprintf("%x-%x-%x-%x-%x", h[0:4], h[4:6], h[6:8], h[8:10], h[10:16])
}

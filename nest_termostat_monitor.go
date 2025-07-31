package main

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
	"os"
	"strings"
	"time"

	"github.com/redis/go-redis/v9"
)

type Config struct {
	ClientID      string `json:"client_id"`
	ClientSecret  string `json:"client_secret"`
	RefreshToken  string `json:"refresh_token"`
	ProjectID     string `json:"project_id"`
	PushoverUser  string `json:"pushover_user"`
	PushoverToken string `json:"pushover_token"`
}

var ctx = context.Background()

func loadConfig(path string) (*Config, error) {
	file, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer file.Close()

	var cfg Config
	if err := json.NewDecoder(file).Decode(&cfg); err != nil {
		return nil, err
	}
	return &cfg, nil
}

func refreshAccessToken(cfg *Config) (string, error) {
	resp, err := http.PostForm("https://oauth2.googleapis.com/token", url.Values{
		"client_id":     {cfg.ClientID},
		"client_secret": {cfg.ClientSecret},
		"refresh_token": {cfg.RefreshToken},
		"grant_type":    {"refresh_token"},
	})
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()

	var tokenResp struct {
		AccessToken string `json:"access_token"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&tokenResp); err != nil {
		return "", err
	}
	return tokenResp.AccessToken, nil
}

func fetchDevices(cfg *Config, token string) ([]map[string]json.RawMessage, error) {
	req, err := http.NewRequest("GET", fmt.Sprintf("https://smartdevicemanagement.googleapis.com/v1/enterprises/%s/devices", cfg.ProjectID), nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Authorization", "Bearer "+token)
	req.Header.Set("Content-Type", "application/json")

	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()

	var result struct {
		Devices []struct {
			Name   string                     `json:"name"`
			Traits map[string]json.RawMessage `json:"traits"`
		} `json:"devices"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&result); err != nil {
		return nil, err
	}

	var devices []map[string]json.RawMessage
	for _, d := range result.Devices {
		traits := d.Traits
		traits["deviceName"] = json.RawMessage(fmt.Sprintf(`"%s"`, d.Name))
		devices = append(devices, traits)
	}
	if len(devices) == 0 {
		alert("N/A", "No devices found", "0", cfg)
		os.Exit(1)
	}
	return devices, nil
}

func cToF(c float64) float64 {
	return (c * 9 / 5) + 32
}

func turnOffThermostat(deviceID string, cfg *Config, token string) {
	deviceName := fmt.Sprintf("enterprises/%s/devices/%s", cfg.ProjectID, deviceID)
	url := fmt.Sprintf("https://smartdevicemanagement.googleapis.com/v1/%s:executeCommand", deviceName)

	payload := map[string]interface{}{
		"command": "sdm.devices.commands.ThermostatMode.SetMode",
		"params":  map[string]string{"mode": "OFF"},
	}
	body, _ := json.Marshal(payload)

	req, _ := http.NewRequest("POST", url, strings.NewReader(string(body)))
	req.Header.Set("Authorization", "Bearer "+token)
	req.Header.Set("Content-Type", "application/json")

	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		alert(deviceID, "Failed to turn off thermostat", "0", cfg)
		return
	}
	defer resp.Body.Close()

	if resp.StatusCode != 200 {
		alert(deviceID, fmt.Sprintf("Thermostat turn-off request returned status %d", resp.StatusCode), "0", cfg)
	} else {
		alert(deviceID, "Thermostat turned off due to emergency alert", "0", cfg)
	}
}

func alert(deviceID, msg, priority string, cfg *Config) {
	data := url.Values{}
	data.Set("token", cfg.PushoverToken)
	data.Set("user", cfg.PushoverUser)
	data.Set("title", "Nest Alert")
	data.Set("message", fmt.Sprintf("%s: %s", deviceID, msg))
	data.Set("priority", priority)
	data.Set("retry", "60")
	data.Set("expire", "3600")

	http.PostForm("https://api.pushover.net/1/messages.json", data)
}

func setupRedis(cfg *Config) *redis.Client {
	rdb := redis.NewClient(&redis.Options{Addr: "localhost:6379"})
	_, err := rdb.Ping(ctx).Result()
	if err != nil {
		alert("N/A", "Failed to connect to Redis", "0", cfg)
		os.Exit(1)
	}
	return rdb
}

func getAccessToken(cfg *Config) string {
	var token string
	var err error

	// Retry up to 3 times total (initial attempt + 2 retries)
	for attempt := 1; attempt <= 3; attempt++ {
		token, err = refreshAccessToken(cfg)
		if err == nil {
			return token
		}

		if attempt < 3 {
			// Wait a bit before retrying (exponential backoff: 1s, 2s)
			time.Sleep(time.Duration(attempt) * time.Second)
		}
	}

	// All attempts failed
	alert("N/A", fmt.Sprintf("Token error after 3 attempts: %s", err.Error()), "0", cfg)
	os.Exit(1)
	return "" // This line will never be reached due to os.Exit(1)
}

func getDevices(cfg *Config, token string) []map[string]json.RawMessage {
	devices, err := fetchDevices(cfg, token)
	if err != nil {
		alert("N/A", "Fetch error:"+err.Error(), "0", cfg)
		os.Exit(1)
	}
	return devices
}

func parseDeviceTraits(traits map[string]json.RawMessage) (deviceID, unit, hvacState string, ambient, heat, cool float64) {
	var name string
	json.Unmarshal(traits["deviceName"], &name)
	parts := strings.Split(name, "/")
	deviceID = parts[len(parts)-1]

	var heatC, coolC, ambientC float64
	if v, ok := traits["sdm.devices.traits.ThermostatTemperatureSetpoint"]; ok {
		var s struct {
			Heat float64 `json:"heatCelsius"`
			Cool float64 `json:"coolCelsius"`
		}
		json.Unmarshal(v, &s)
		heatC = s.Heat
		coolC = s.Cool
	}
	if v, ok := traits["sdm.devices.traits.ThermostatHvac"]; ok {
		var s struct {
			Status string `json:"status"`
		}
		json.Unmarshal(v, &s)
		hvacState = s.Status
	}
	if v, ok := traits["sdm.devices.traits.Temperature"]; ok {
		var s struct {
			Ambient float64 `json:"ambientTemperatureCelsius"`
		}
		json.Unmarshal(v, &s)
		ambientC = s.Ambient
	}
	if v, ok := traits["sdm.devices.traits.Settings"]; ok {
		var s struct {
			DisplayTempUnit string `json:"displayTemperatureUnit"`
		}
		json.Unmarshal(v, &s)
		unit = s.DisplayTempUnit
	}

	ambient = ambientC
	heat = heatC
	cool = coolC
	if unit == "FAHRENHEIT" {
		ambient = cToF(ambientC)
		heat = cToF(heatC)
		cool = cToF(coolC)
	}
	return
}

func handleDeviceSamples(rdb *redis.Client, deviceID string, ambient, heat, cool float64, hvacState string, cfg *Config, token string) {
	key := fmt.Sprintf("nest:%s:temps", deviceID)

	sample := map[string]interface{}{
		"ambient":    ambient,
		"hvac_state": hvacState,
		"heat":       heat,
		"cool":       cool,
		"ts":         time.Now().Format(time.RFC3339),
	}
	data, _ := json.Marshal(sample)
	rdb.LPush(ctx, key, data)
	rdb.LTrim(ctx, key, 0, 2)

	samples, _ := rdb.LRange(ctx, key, 0, 2).Result()
	if len(samples) == 3 {
		var s0, s1, s2 map[string]interface{}
		json.Unmarshal([]byte(samples[0]), &s0) // newest
		json.Unmarshal([]byte(samples[1]), &s1)
		json.Unmarshal([]byte(samples[2]), &s2) // oldest

		a0 := s0["ambient"].(float64)
		a1 := s1["ambient"].(float64)
		a2 := s2["ambient"].(float64)

		hvac0 := s0["hvac_state"].(string)
		hvac1 := s1["hvac_state"].(string)
		hvac2 := s2["hvac_state"].(string)

		if hvac0 == "COOLING" && hvac1 == "COOLING" && hvac2 == "COOLING" {
			if a0 > a1 && a1 > a2 {
				alert(deviceID, fmt.Sprintf("COOLING: ambient consistently rising (%.1f → %.1f → %.1f)", a2, a1, a0), "2", cfg)
			}
		}
		if hvac0 == "HEATING" && hvac1 == "HEATING" && hvac2 == "HEATING" {
			if a0 < a1 && a1 < a2 {
				alert(deviceID, fmt.Sprintf("HEATING: ambient consistently falling (%.1f → %.1f → %.1f)", a2, a1, a0), "2", cfg)
				turnOffThermostat(deviceID, cfg, token)
			}
		}
	}
}

func processDevices(rdb *redis.Client, devices []map[string]json.RawMessage, cfg *Config, token string) {
	for _, traits := range devices {
		deviceID, _, hvacState, ambient, heat, cool := parseDeviceTraits(traits)
		handleDeviceSamples(rdb, deviceID, ambient, heat, cool, hvacState, cfg, token)
	}
}

func main() {
	cfg, err := loadConfig("config.json")
	if err != nil {
		alert("N/A", "Failed to load config:"+err.Error(), "0", cfg)
		os.Exit(1)
	}

	rdb := setupRedis(cfg)
	token := getAccessToken(cfg)
	devices := getDevices(cfg, token)
	processDevices(rdb, devices, cfg, token)
}

// Package weather fetches hourly outdoor temperatures for the room the box
// heats (config thermal). One source today: Open-Meteo — free, no API key,
// worldwide by latitude/longitude, hourly history up to ~3 months back.
package weather

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
	"time"
)

// Hour is one hourly outdoor temperature. HourMS is the hour's start, unix ms
// UTC — the same bucketing the activity log's hourly energy uses.
type Hour struct {
	HourMS int64
	TempF  float64
}

// UserAgent identifies corrallm to the weather service — the project, never an
// operator's personal contact details.
const UserAgent = "corrallm (https://github.com/IodeSystems/corrallm)"

// BaseURL is Open-Meteo's forecast endpoint; a variable so tests can point it
// at a fake.
var BaseURL = "https://api.open-meteo.com/v1/forecast"

// FetchOpenMeteo returns hourly temperatures (°F) at lat/lon for the past
// pastDays days through the end of today (UTC). past_days tops out at 92.
func FetchOpenMeteo(ctx context.Context, client *http.Client, lat, lon float64, pastDays int) ([]Hour, error) {
	if pastDays < 0 {
		pastDays = 0
	}
	if pastDays > 92 {
		pastDays = 92
	}
	q := url.Values{}
	q.Set("latitude", fmt.Sprintf("%.4f", lat))
	q.Set("longitude", fmt.Sprintf("%.4f", lon))
	q.Set("hourly", "temperature_2m")
	q.Set("temperature_unit", "fahrenheit")
	q.Set("timezone", "GMT")
	q.Set("timeformat", "unixtime")
	q.Set("past_days", fmt.Sprint(pastDays))
	q.Set("forecast_days", "1")
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, BaseURL+"?"+q.Encode(), nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("User-Agent", UserAgent)
	if client == nil {
		client = &http.Client{Timeout: 20 * time.Second}
	}
	resp, err := client.Do(req)
	if err != nil {
		return nil, err
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("open-meteo: HTTP %d", resp.StatusCode)
	}
	var body struct {
		Hourly struct {
			Time []int64    `json:"time"`
			Temp []*float64 `json:"temperature_2m"`
		} `json:"hourly"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&body); err != nil {
		return nil, fmt.Errorf("open-meteo: %w", err)
	}
	if len(body.Hourly.Time) != len(body.Hourly.Temp) {
		return nil, fmt.Errorf("open-meteo: %d times but %d temperatures", len(body.Hourly.Time), len(body.Hourly.Temp))
	}
	out := make([]Hour, 0, len(body.Hourly.Time))
	for i, t := range body.Hourly.Time {
		if body.Hourly.Temp[i] == nil {
			continue // a gap in the model output: leave the hour unknown, not 0°F
		}
		out = append(out, Hour{HourMS: t * 1000, TempF: *body.Hourly.Temp[i]})
	}
	return out, nil
}

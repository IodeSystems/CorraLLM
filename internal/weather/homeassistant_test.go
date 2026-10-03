package weather

import (
	"context"
	"math"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

// TestFetchHAHourly: values are weighted by how long they held within each
// hour, unavailable states contribute nothing, °C is converted, and the token
// goes in a Bearer header.
func TestFetchHAHourly(t *testing.T) {
	var gotAuth, gotPath string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotAuth, gotPath = r.Header.Get("Authorization"), r.URL.String()
		_, _ = w.Write([]byte(`[[
		 {"state":"20","last_changed":"2026-10-02T09:30:00Z","attributes":{"unit_of_measurement":"°C"}},
		 {"state":"25","last_changed":"2026-10-02T10:15:00Z"},
		 {"state":"unavailable","last_changed":"2026-10-02T10:45:00Z"},
		 {"state":"30","last_changed":"2026-10-02T11:00:00Z"}
		]]`))
	}))
	defer srv.Close()
	from := time.Date(2026, 10, 2, 10, 0, 0, 0, time.UTC)
	to := time.Date(2026, 10, 2, 12, 0, 0, 0, time.UTC)
	hours, err := FetchHAHourly(context.Background(), nil, srv.URL+"/", "tok", "sensor.office", from, to)
	if err != nil {
		t.Fatal(err)
	}
	if gotAuth != "Bearer tok" || !strings.Contains(gotPath, "filter_entity_id=sensor.office") {
		t.Errorf("auth %q path %q", gotAuth, gotPath)
	}
	// 10:00–11:00: 20°C for 15 min, 25°C for 30 min, unavailable for 15 →
	// (20*15 + 25*30)/45 = 23.33°C = 74.0°F. 11:00–12:00: 30°C = 86°F.
	if len(hours) != 2 {
		t.Fatalf("hours = %+v", hours)
	}
	if want := (20.0*15+25*30)/45*9/5 + 32; math.Abs(hours[0].TempF-want) > 1e-9 {
		t.Errorf("10:00 = %v, want %v", hours[0].TempF, want)
	}
	if hours[1].TempF != 86 {
		t.Errorf("11:00 = %v, want 86", hours[1].TempF)
	}
}

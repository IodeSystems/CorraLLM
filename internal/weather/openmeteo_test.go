package weather

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// TestFetchOpenMeteo parses unixtime hours, skips null temperatures instead of
// reading them as 0°F, asks for °F in GMT, and identifies as corrallm.
func TestFetchOpenMeteo(t *testing.T) {
	var gotUA, gotQuery string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotUA, gotQuery = r.UserAgent(), r.URL.RawQuery
		_, _ = w.Write([]byte(`{"hourly":{"time":[1790960400,1790964000,1790967600],"temperature_2m":[62.9,null,67.3]}}`))
	}))
	defer srv.Close()
	old := BaseURL
	BaseURL = srv.URL
	defer func() { BaseURL = old }()

	hours, err := FetchOpenMeteo(context.Background(), nil, 47.61, -122.33, 31)
	if err != nil {
		t.Fatal(err)
	}
	if len(hours) != 2 || hours[0].HourMS != 1790960400000 || hours[0].TempF != 62.9 || hours[1].TempF != 67.3 {
		t.Errorf("hours = %+v", hours)
	}
	if gotUA != UserAgent {
		t.Errorf("User-Agent = %q", gotUA)
	}
	for _, want := range []string{"temperature_unit=fahrenheit", "timezone=GMT", "timeformat=unixtime", "past_days=31", "latitude=47.6100"} {
		if !strings.Contains(gotQuery, want) {
			t.Errorf("query %q lacks %q", gotQuery, want)
		}
	}
}

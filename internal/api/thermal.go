package api

import (
	"context"
	"fmt"
	"regexp"

	"github.com/danielgtaylor/huma/v2"

	"github.com/iodesystems/corrallm/internal/config"
	"github.com/iodesystems/corrallm/internal/cost"
	"github.com/iodesystems/corrallm/internal/store"
)

// SetThermalInput is the room the box heats: its room and outdoor temperatures,
// the targets it is held at, and what a therm of heat costs to remove or is worth
// to add. See config.Thermal for how the factors are derived.
type SetThermalInput struct {
	Body struct {
		Clear              bool    `json:"clear,omitempty" doc:"Remove the thermal settings; the dashboard goes back to reporting heat in BTU only."`
		RoomF              float64 `json:"roomF" doc:"Room (indoor) temperature, °F, as you read it. Decides the mode."`
		OutdoorF           float64 `json:"outdoorF,omitempty" doc:"Outdoor temperature, °F. Needed when cooling is a fan: the room-to-outdoor gap sets what it costs. With a location it is only the fallback for hours with no weather."`
		Latitude           float64 `json:"latitude,omitempty" doc:"Location for hourly outdoor temperatures (Open-Meteo, no key). Set both to price cooling hour by hour."`
		Longitude          float64 `json:"longitude,omitempty" doc:"See latitude."`
		HomeAssistantURL   string  `json:"homeAssistantUrl,omitempty" doc:"Home Assistant base URL (https://ha.example.com) for hourly room (and optionally outdoor) temperatures; empty for the manual readings."`
		HomeAssistantToken string  `json:"homeAssistantToken,omitempty" doc:"A ${KEY} reference to a long-lived access token in the credential store (~/.corrallm/credentials) — never the token itself."`
		RoomEntity         string  `json:"roomEntity,omitempty" doc:"Temperature sensor for the room the box is in (or a whole-home aggregate when air moves between rooms), e.g. sensor.office_temperature."`
		OutdoorEntity      string  `json:"outdoorEntity,omitempty" doc:"Optional outdoor temperature sensor; preferred over the weather service."`
		CoolTargetF        float64 `json:"coolTargetF" doc:"Cooling target, °F. Above it the room is being cooled."`
		CoolCostPerTherm   float64 `json:"coolCostPerTherm,omitempty" minimum:"0" doc:"Fixed $ to remove one therm (100,000 BTU) of heat — e.g. an AC at $/kWh x 29.31 / COP. Ignored when coolFanCfm is set."`
		CoolFanCFM         float64 `json:"coolFanCfm,omitempty" minimum:"0" doc:"Cooling is a fan moving outside air: its airflow, CFM. Set this and coolFanWatts to have the cost computed from the room-to-outdoor gap."`
		CoolFanWatts       float64 `json:"coolFanWatts,omitempty" minimum:"0" doc:"The cooling fan's draw, W."`
		HeatTargetF        float64 `json:"heatTargetF" doc:"Heating target, °F. Below it the room is being heated. Must be below coolTargetF."`
		HeatCostPerTherm   float64 `json:"heatCostPerTherm" minimum:"0" doc:"$ one therm of heat is worth when the room is heated — the marginal source the thermostat turns down."`
		Notes              string  `json:"notes,omitempty" doc:"Free text kept with the settings (how the factors were worked out)."`
	}
}

// SetThermal stores the room settings that turn heat into a cooling cost or a
// heating saving.
func (h *Handlers) SetThermal(ctx context.Context, in *SetThermalInput) (*ConfigMutationOutput, error) {
	b := in.Body
	if !b.Clear {
		if b.HeatTargetF <= 0 || b.CoolTargetF <= 0 {
			return nil, huma.Error400BadRequest("both targets are required, in °F")
		}
		if b.HeatTargetF >= b.CoolTargetF {
			return nil, huma.Error400BadRequest("the heating target must be below the cooling target — between them neither system runs")
		}
		if b.CoolFanCFM > 0 && b.CoolFanWatts <= 0 {
			return nil, huma.Error400BadRequest("a cooling fan needs its watts as well as its CFM")
		}
		if b.HomeAssistantURL != "" || b.RoomEntity != "" || b.OutdoorEntity != "" {
			if b.HomeAssistantURL == "" || b.RoomEntity == "" {
				return nil, huma.Error400BadRequest("Home Assistant needs its URL and the room sensor's entity id")
			}
			// The config is served over HTTP and quoted in docs: a token pasted
			// here would be published. Only a reference into the store.
			if !secretRef.MatchString(b.HomeAssistantToken) {
				return nil, huma.Error400BadRequest("homeAssistantToken must be a ${KEY} reference to the credential store (e.g. ${HA_TOKEN}), not the token itself")
			}
		}
	}
	cooling := fmt.Sprintf("$%.2f/therm", b.CoolCostPerTherm)
	if b.CoolFanCFM > 0 {
		cooling = fmt.Sprintf("a %.0f CFM / %.0f W fan", b.CoolFanCFM, b.CoolFanWatts)
	}
	outdoor := fmt.Sprintf("outdoor %.0f°F", b.OutdoorF)
	if b.Latitude != 0 || b.Longitude != 0 {
		outdoor = fmt.Sprintf("outdoor hourly from weather at %.2f,%.2f", b.Latitude, b.Longitude)
	}
	what := fmt.Sprintf("room %.0f°F, %s; cool above %.0f°F with %s, heat below %.0f°F at $%.2f/therm",
		b.RoomF, outdoor, b.CoolTargetF, cooling, b.HeatTargetF, b.HeatCostPerTherm)
	if b.Clear {
		what = "thermal settings cleared — heat is reported in BTU only"
	}
	err := h.mutateConfig(ctx, what, func(c *config.Config) error {
		if b.Clear {
			c.Thermal = nil
			return nil
		}
		c.Thermal = &config.Thermal{RoomF: b.RoomF, OutdoorF: b.OutdoorF, Latitude: b.Latitude, Longitude: b.Longitude, CoolTargetF: b.CoolTargetF,
			CoolCostPerTherm: b.CoolCostPerTherm, CoolFanCFM: b.CoolFanCFM, CoolFanWatts: b.CoolFanWatts,
			HeatTargetF: b.HeatTargetF, HeatCostPerTherm: b.HeatCostPerTherm, Notes: b.Notes}
		if b.HomeAssistantURL != "" {
			c.Thermal.HomeAssistant = &config.HomeAssistant{URL: b.HomeAssistantURL, Token: b.HomeAssistantToken,
				RoomEntity: b.RoomEntity, OutdoorEntity: b.OutdoorEntity}
		}
		return nil
	})
	if err != nil {
		return nil, err
	}
	out := &ConfigMutationOutput{}
	out.Body.OK = true
	out.Body.Message = what
	return out, nil
}

// hourlyThermal prices the window's heat hour by hour. Each hour takes its own
// ROOM temperature (a Home Assistant sensor's hourly mean, else the manual
// roomF) — which decides that hour's mode — and its own OUTDOOR temperature
// (the cached weather, else the manual outdoorF), which decides what a fan's
// cooling costs. A window longer than an hour mixes night and day, heating and
// cooling, so one window-wide figure would misprice it. ok is false when
// neither a location nor a room sensor is configured (the single-temperature
// path applies).
//
// Only energy-billed rows heat this room: a paid or remote backend's tokens
// warmed somebody else's.
func (h *Handlers) hourlyThermal(win store.Window, th *config.Thermal, known map[string]config.Model,
	cm *cost.Model, rate float64) (perServed map[string]float64, st thermalHourStats, ok bool) {
	if !(th.HasHourlyOutdoor() || th.HasRoomSensor()) || h.Store == nil || rate <= 0 {
		return nil, st, false
	}
	costs, err := h.Store.CostByHour(win)
	if err != nil {
		return nil, st, false
	}
	perServed = map[string]float64{}
	if len(costs) == 0 {
		return perServed, st, true
	}
	lo, hi := costs[0].HourMS, costs[0].HourMS
	for _, c := range costs {
		lo, hi = min(lo, c.HourMS), max(hi, c.HourMS)
	}
	end := hi + 3_600_000
	wx, room := map[int64]float64{}, map[int64]float64{}
	if th.HasHourlyOutdoor() {
		if wx, err = h.Store.WeatherBetween(th.OutdoorLoc(), lo, end); err != nil {
			return nil, st, false
		}
	}
	if th.HasRoomSensor() {
		if room, err = h.Store.WeatherBetween(th.RoomLoc(), lo, end); err != nil {
			return nil, st, false
		}
	}
	priced, blocked, missing := map[int64]bool{}, map[int64]bool{}, map[int64]bool{}
	modes := map[int64]string{}
	firstOut, firstRoom := true, true
	for _, c := range costs {
		if classify(known, cm, c.Served).Billing != BillEnergy {
			continue
		}
		btu := c.CostUSD / rate * btuPerKwh

		roomF, haveRoom := room[c.HourMS]
		if haveRoom {
			if firstRoom {
				st.RoomMinF, st.RoomMaxF, firstRoom = roomF, roomF, false
			}
			st.RoomMinF, st.RoomMaxF = min(st.RoomMinF, roomF), max(st.RoomMaxF, roomF)
		} else {
			roomF = th.RoomF
			if th.HasRoomSensor() {
				st.HoursRoomFallback++
			}
		}
		outF, haveOut := wx[c.HourMS]
		if haveOut {
			if firstOut {
				st.OutdoorMinF, st.OutdoorMaxF, firstOut = outF, outF, false
			}
			st.OutdoorMinF, st.OutdoorMaxF = min(st.OutdoorMinF, outF), max(st.OutdoorMaxF, outF)
		}

		mode := th.ModeAt(roomF)
		modes[c.HourMS] = mode
		switch mode {
		case "cooling":
			if !haveOut {
				if th.HasHourlyOutdoor() {
					missing[c.HourMS] = true
				}
				if th.OutdoorF == 0 {
					continue
				}
				outF = th.OutdoorF // the manual reading stands in
			}
			f, can := th.CoolFactorFor(roomF, outF, rate)
			if !can {
				blocked[c.HourMS] = true
				continue
			}
			perServed[c.Served] += btu / btuPerTherm * f
			priced[c.HourMS] = true
		case "heating":
			perServed[c.Served] -= btu / btuPerTherm * th.HeatCostPerTherm
			priced[c.HourMS] = true
		}
	}
	for _, m := range modes {
		switch m {
		case "cooling":
			st.HoursCooling++
		case "heating":
			st.HoursHeating++
		default:
			st.HoursNeutral++
		}
	}
	st.HoursPriced, st.HoursBlocked, st.HoursNoWeather = len(priced), len(blocked), len(missing)
	return perServed, st, true
}

// thermalHourStats describes an hourly-priced window: hours by mode, how many
// carried a figure, how many a fan could not cool (warmer outside than the
// room), how many had no cached outdoor weather, and the room and outdoor
// ranges over hours with heat. HoursRoomFallback counts heat-hours the room
// sensor had no reading for (the manual roomF stood in).
type thermalHourStats struct {
	HoursPriced, HoursBlocked, HoursNoWeather int
	HoursCooling, HoursHeating, HoursNeutral  int
	HoursRoomFallback                         int
	OutdoorMinF, OutdoorMaxF                  float64
	RoomMinF, RoomMaxF                        float64
}

// secretRef is a whole-value ${KEY} credential reference.
var secretRef = regexp.MustCompile(`^\$\{[A-Za-z_][A-Za-z0-9_]*\}$`)

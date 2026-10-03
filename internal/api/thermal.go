package api

import (
	"context"
	"fmt"

	"github.com/danielgtaylor/huma/v2"

	"github.com/iodesystems/corrallm/internal/config"
)

// SetThermalInput is the room the box heats: its room and outdoor temperatures,
// the targets it is held at, and what a therm of heat costs to remove or is worth
// to add. See config.Thermal for how the factors are derived.
type SetThermalInput struct {
	Body struct {
		Clear            bool    `json:"clear,omitempty" doc:"Remove the thermal settings; the dashboard goes back to reporting heat in BTU only."`
		RoomF            float64 `json:"roomF" doc:"Room (indoor) temperature, °F, as you read it. Decides the mode."`
		OutdoorF         float64 `json:"outdoorF,omitempty" doc:"Outdoor temperature, °F. Needed when cooling is a fan: the room-to-outdoor gap sets what it costs."`
		CoolTargetF      float64 `json:"coolTargetF" doc:"Cooling target, °F. Above it the room is being cooled."`
		CoolCostPerTherm float64 `json:"coolCostPerTherm,omitempty" minimum:"0" doc:"Fixed $ to remove one therm (100,000 BTU) of heat — e.g. an AC at $/kWh x 29.31 / COP. Ignored when coolFanCfm is set."`
		CoolFanCFM       float64 `json:"coolFanCfm,omitempty" minimum:"0" doc:"Cooling is a fan moving outside air: its airflow, CFM. Set this and coolFanWatts to have the cost computed from the room-to-outdoor gap."`
		CoolFanWatts     float64 `json:"coolFanWatts,omitempty" minimum:"0" doc:"The cooling fan's draw, W."`
		HeatTargetF      float64 `json:"heatTargetF" doc:"Heating target, °F. Below it the room is being heated. Must be below coolTargetF."`
		HeatCostPerTherm float64 `json:"heatCostPerTherm" minimum:"0" doc:"$ one therm of heat is worth when the room is heated — the marginal source the thermostat turns down."`
		Notes            string  `json:"notes,omitempty" doc:"Free text kept with the settings (how the factors were worked out)."`
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
	}
	cooling := fmt.Sprintf("$%.2f/therm", b.CoolCostPerTherm)
	if b.CoolFanCFM > 0 {
		cooling = fmt.Sprintf("a %.0f CFM / %.0f W fan", b.CoolFanCFM, b.CoolFanWatts)
	}
	what := fmt.Sprintf("room %.0f°F, outdoor %.0f°F; cool above %.0f°F with %s, heat below %.0f°F at $%.2f/therm",
		b.RoomF, b.OutdoorF, b.CoolTargetF, cooling, b.HeatTargetF, b.HeatCostPerTherm)
	if b.Clear {
		what = "thermal settings cleared — heat is reported in BTU only"
	}
	err := h.mutateConfig(ctx, what, func(c *config.Config) error {
		if b.Clear {
			c.Thermal = nil
			return nil
		}
		c.Thermal = &config.Thermal{RoomF: b.RoomF, OutdoorF: b.OutdoorF, CoolTargetF: b.CoolTargetF,
			CoolCostPerTherm: b.CoolCostPerTherm, CoolFanCFM: b.CoolFanCFM, CoolFanWatts: b.CoolFanWatts,
			HeatTargetF: b.HeatTargetF, HeatCostPerTherm: b.HeatCostPerTherm, Notes: b.Notes}
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

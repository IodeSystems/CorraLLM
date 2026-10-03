package api

import (
	"context"
	"fmt"

	"github.com/danielgtaylor/huma/v2"

	"github.com/iodesystems/corrallm/internal/config"
)

// SetThermalInput is the room the box heats: its ambient temperature, the
// targets it is held at, and what a therm of heat costs to remove or is worth
// to add. See config.Thermal for how the factors are derived.
type SetThermalInput struct {
	Body struct {
		Clear            bool    `json:"clear,omitempty" doc:"Remove the thermal settings; the dashboard goes back to reporting heat in BTU only."`
		AmbientF         float64 `json:"ambientF" doc:"Room ambient temperature, °F, as you read it."`
		CoolTargetF      float64 `json:"coolTargetF" doc:"Cooling target, °F. Above it the room is being cooled."`
		CoolCostPerTherm float64 `json:"coolCostPerTherm" minimum:"0" doc:"$ to remove one therm (100,000 BTU) of heat."`
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
	}
	what := fmt.Sprintf("room set to %.0f°F ambient; cool above %.0f°F at $%.2f/therm, heat below %.0f°F at $%.2f/therm",
		b.AmbientF, b.CoolTargetF, b.CoolCostPerTherm, b.HeatTargetF, b.HeatCostPerTherm)
	if b.Clear {
		what = "thermal settings cleared — heat is reported in BTU only"
	}
	err := h.mutateConfig(ctx, what, func(c *config.Config) error {
		if b.Clear {
			c.Thermal = nil
			return nil
		}
		c.Thermal = &config.Thermal{AmbientF: b.AmbientF, CoolTargetF: b.CoolTargetF, CoolCostPerTherm: b.CoolCostPerTherm,
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

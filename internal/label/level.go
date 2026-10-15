package label

import (
	"fmt"
	"math"
)

// Level is one fuzzy band. Exactly one shape is set.
type Level struct {
	Name string `json:"name" yaml:"name"`
	// Up is 0 at or below Up[0] and 1 at or above Up[1].
	Up []float64 `json:"up,omitempty" yaml:"up,omitempty"`
	// Down is 1 at or below Down[0] and 0 at or above Down[1].
	Down []float64 `json:"down,omitempty" yaml:"down,omitempty"`
	// Band is a trapezoid: rising from [0] to [1], flat to [2], falling to [3].
	Band []float64 `json:"band,omitempty" yaml:"band,omitempty"`
}

// degree is how much a value belongs to the band, from 0 to 1.
func (l Level) degree(v float64) float64 {
	ramp := func(a, b float64) float64 {
		switch {
		case v <= a:
			return 0
		case v >= b:
			return 1
		default:
			return (v - a) / (b - a)
		}
	}
	switch {
	case len(l.Up) == 2:
		return ramp(l.Up[0], l.Up[1])
	case len(l.Down) == 2:
		return 1 - ramp(l.Down[0], l.Down[1])
	case len(l.Band) == 4:
		if v < l.Band[1] {
			return ramp(l.Band[0], l.Band[1])
		}
		return 1 - ramp(l.Band[2], l.Band[3])
	}
	return 0
}

// Degrees is every band a value belongs to at all, by how much.
type Degrees map[string]float64

// Strongest is the band the value belongs to most, and its index among the rule's levels; -1 when
// the rule has no levels.
func (r Rule) Strongest(v float64) (string, int) {
	best, name, index := 0.0, "", -1
	for i, l := range r.Levels {
		if d := l.degree(v); d > 0 && d >= best {
			best, name, index = d, l.Name, i
		}
	}
	return name, index
}

// degrees is every band a value belongs to, rounded to two places.
func (r Rule) degrees(v float64) Degrees {
	out := Degrees{}
	for _, l := range r.Levels {
		if d := l.degree(v); d > 0 {
			out[l.Name] = math.Round(d*100) / 100
		}
	}
	return out
}

// validateLevels checks the bands are well formed and that together they cover every value: a gap
// between two bands is a value that belongs to nothing and would be read as having no level.
func validateLevels(levels []Level) error {
	names := map[string]bool{}
	for _, l := range levels {
		if names[l.Name] {
			return fmt.Errorf("level %s twice", l.Name)
		}
		names[l.Name] = true
		var bounds []float64
		switch {
		case len(l.Up) == 2 && l.Down == nil && l.Band == nil:
			bounds = l.Up
		case len(l.Down) == 2 && l.Up == nil && l.Band == nil:
			bounds = l.Down
		case len(l.Band) == 4 && l.Up == nil && l.Down == nil:
			bounds = l.Band
		default:
			return fmt.Errorf("level %s needs exactly one of up[2], down[2], band[4]", l.Name)
		}
		for i := 1; i < len(bounds); i++ {
			if bounds[i] < bounds[i-1] {
				return fmt.Errorf("level %s bounds go backwards", l.Name)
			}
		}
	}
	if len(levels) == 0 {
		return nil
	}
	first, last := levels[0], levels[len(levels)-1]
	if first.Down == nil {
		return fmt.Errorf("the lowest level, %s, must be a down ramp so every low value has a band", first.Name)
	}
	if last.Up == nil {
		return fmt.Errorf("the highest level, %s, must be an up ramp so every high value has a band", last.Name)
	}
	// Each neighbouring pair must leave no value between them with no band at all.
	for i := 1; i < len(levels); i++ {
		if fade, rise := upperEdge(levels[i-1]), lowerEdge(levels[i]); rise >= fade {
			return fmt.Errorf("levels %s and %s leave a gap between %g and %g", levels[i-1].Name, levels[i].Name, fade, rise)
		}
	}
	return nil
}

// upperEdge is where a band stops covering anything going up; lowerEdge where it starts.
func upperEdge(l Level) float64 {
	switch {
	case l.Down != nil:
		return l.Down[1]
	case l.Band != nil:
		return l.Band[3]
	}
	return math.Inf(1)
}

func lowerEdge(l Level) float64 {
	switch {
	case l.Up != nil:
		return l.Up[0]
	case l.Band != nil:
		return l.Band[0]
	}
	return math.Inf(-1)
}

package dip

import "time"

type Point struct {
	Date time.Time
	NAV  float64
}

func Peak(points []Point, window int) (Point, bool) {
	if len(points) == 0 || window < 1 {
		return Point{}, false
	}
	if len(points) > window {
		points = points[len(points)-window:]
	}
	best := points[0]
	for _, p := range points[1:] {
		if p.NAV > best.NAV {
			best = p
		}
	}
	return best, true
}

func DropPct(peak, current float64) float64 {
	if peak <= 0 || current >= peak {
		return 0
	}
	return (peak - current) / peak * 100
}

func Level(drop float64, triggers []float64) float64 {
	var lvl float64
	for _, t := range triggers {
		if drop >= t {
			lvl = t
		}
	}
	return lvl
}

func ShouldAlert(level, lastLevel float64) bool { return level > lastLevel }

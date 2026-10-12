package dip

import (
	"testing"
	"time"
)

func pts(v ...float64) []Point {
	out := make([]Point, len(v))
	for i, n := range v {
		out[i] = Point{time.Date(2026, 1, 1+i, 0, 0, 0, 0, time.UTC), n}
	}
	return out
}

func TestPeakWindow(t *testing.T) {
	p, ok := Peak(pts(10, 20, 15, 12), 3)
	if !ok || p.NAV != 20 {
		t.Fatalf("got %v %v", p, ok)
	}
	p, _ = Peak(pts(10, 20, 15, 12), 2) // 20 falls out of window
	if p.NAV != 15 {
		t.Fatalf("window not applied: %v", p)
	}
	p, ok = Peak(pts(10, 12), 252) // fewer points than window
	if !ok || p.NAV != 12 {
		t.Fatalf("short history: %v %v", p, ok)
	}
	if _, ok := Peak(nil, 5); ok {
		t.Fatal("empty must be !ok")
	}
}

func TestDropPct(t *testing.T) {
	if got := DropPct(100, 90); got != 10 {
		t.Errorf("got %v", got)
	}
	for _, c := range [][2]float64{{100, 100}, {100, 110}, {0, 5}} {
		if got := DropPct(c[0], c[1]); got != 0 {
			t.Errorf("DropPct(%v,%v)=%v want 0", c[0], c[1], got)
		}
	}
}

func TestLevelAndAlert(t *testing.T) {
	tr := []float64{5, 10, 15}
	cases := []struct{ drop, want float64 }{{0, 0}, {4.99, 0}, {5, 5}, {12, 10}, {40, 15}}
	for _, c := range cases {
		if got := Level(c.drop, tr); got != c.want {
			t.Errorf("Level(%v)=%v want %v", c.drop, got, c.want)
		}
	}
	if !ShouldAlert(10, 5) || ShouldAlert(10, 10) || ShouldAlert(5, 10) || ShouldAlert(0, 0) {
		t.Fatal("ShouldAlert wrong")
	}
}

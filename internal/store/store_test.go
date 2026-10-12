package store

import (
	"context"
	"testing"
	"time"

	"github.com/pallat/iwealth/internal/config"
)

func open(t *testing.T) *Store {
	t.Helper()
	s, err := Open(":memory:")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { s.Close() })
	return s
}

func d(m time.Month, day int) time.Time { return time.Date(2026, m, day, 0, 0, 0, 0, time.UTC) }

func TestNAVUpsertAndHistory(t *testing.T) {
	s, ctx := open(t), context.Background()
	s.UpsertNAV(ctx, "A", d(1, 2), 11)
	s.UpsertNAV(ctx, "A", d(1, 1), 10)
	s.UpsertNAV(ctx, "A", d(1, 2), 12) // overwrite, no duplicate
	h, err := s.History(ctx, "A")
	if err != nil || len(h) != 2 || h[0].NAV != 10 || h[1].NAV != 12 {
		t.Fatalf("%v %v", h, err)
	}
	if ok, _ := s.HasNAV(ctx, "A", d(1, 2)); !ok {
		t.Fatal("HasNAV false")
	}
	if ok, _ := s.HasNAV(ctx, "A", d(1, 3)); ok {
		t.Fatal("HasNAV true for missing")
	}
}

func TestLevel(t *testing.T) {
	s, ctx := open(t), context.Background()
	if l, _ := s.LastLevel(ctx, "A"); l != 0 {
		t.Fatalf("default %v", l)
	}
	s.SetLevel(ctx, "A", 10)
	s.SetLevel(ctx, "A", 15)
	if l, _ := s.LastLevel(ctx, "A"); l != 15 {
		t.Fatalf("got %v", l)
	}
}

func TestPaidByTypeYearScoped(t *testing.T) {
	s, ctx := open(t), context.Background()
	s.AddPaid(ctx, "A", config.RMF, 1000, d(3, 1))
	s.AddPaid(ctx, "A", config.RMF, 500, d(4, 1))
	s.AddPaid(ctx, "B", config.SSF, 200, d(5, 1))
	s.AddPaid(ctx, "A", config.RMF, 9999, time.Date(2025, 12, 31, 0, 0, 0, 0, time.UTC))
	m, err := s.PaidByType(ctx, 2026)
	if err != nil || m[config.RMF] != 1500 || m[config.SSF] != 200 {
		t.Fatalf("%v %v", m, err)
	}
}

func TestDeductionsSetReplaceClear(t *testing.T) {
	s := open(t)
	ctx := context.Background()
	s.SetDeduction(ctx, 2026, "pvd", 1000)
	s.SetDeduction(ctx, 2026, "pvd", 2500)
	s.SetDeduction(ctx, 2025, "pvd", 9999)
	m, err := s.Deductions(ctx, 2026)
	if err != nil || len(m) != 1 || m["pvd"] != 2500 {
		t.Fatalf("%v %v", m, err)
	}
	s.SetDeduction(ctx, 2026, "pvd", 0)
	if m, _ = s.Deductions(ctx, 2026); len(m) != 0 {
		t.Fatalf("not cleared: %v", m)
	}
}

func TestWithheld(t *testing.T) {
	s := open(t)
	ctx := context.Background()
	if v, err := s.Withheld(ctx, 2026); err != nil || v != 0 {
		t.Fatalf("empty: %v %v", v, err)
	}
	s.SetWithheld(ctx, 2026, 1000)
	s.SetWithheld(ctx, 2026, 2500)
	s.SetWithheld(ctx, 2025, 9999)
	if v, err := s.Withheld(ctx, 2026); err != nil || v != 2500 {
		t.Fatalf("%v %v", v, err)
	}
	s.SetWithheld(ctx, 2026, 0)
	if v, _ := s.Withheld(ctx, 2026); v != 0 {
		t.Fatalf("not cleared: %v", v)
	}
}

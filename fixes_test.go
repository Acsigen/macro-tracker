package main

import (
	"context"
	"net/http"
	"net/url"
	"reflect"
	"strings"
	"testing"
	"time"
)

func TestReadingDateConflictPreservesBothRecords(t *testing.T) {
	for _, kind := range []string{"body", "sleep"} {
		t.Run(kind, func(t *testing.T) {
			a := testApp(t)
			h := a.routes()
			for _, day := range []string{"2026-01-01", "2026-01-02"} {
				doForm(t, h, "POST", "/"+kind, readingForm(kind, day), true, 303)
			}
			doForm(t, h, "POST", "/"+kind+"/1", readingForm(kind, "2026-01-02"), true, http.StatusConflict)
			for id, want := range map[int]string{1: "2026-01-01", 2: "2026-01-02"} {
				var got string
				if err := a.db.QueryRow("SELECT entry_date FROM "+kind+"_entries WHERE id=?", id).Scan(&got); err != nil || got != want {
					t.Fatalf("conflict changed record %d: %q %v", id, got, err)
				}
			}
		})
	}
}

func TestFoodSelectionChangesSnapshotExplicitly(t *testing.T) {
	a := testApp(t)
	h := a.routes()
	seedFood(t, a, foodForm(), 0)
	logReviewedFood(t, a, "/nutrition/entries", entryForm())
	v := foodForm()
	v.Set("name", "Other food")
	v.Set("carbohydrate", "20")
	seedFood(t, a, v, 0)
	e := entryForm()
	e.Set("food_name", "Other food")
	logReviewedFood(t, a, "/nutrition/entries/1", e)
	entries, err := a.listFoodEntries(context.Background(), 10)
	if err != nil || len(entries) != 1 || entries[0].FoodID != 2 || entries[0].FoodName != "Other food" || entries[0].Carbohydrate != 20 {
		t.Fatalf("explicit food switch: %+v %v", entries, err)
	}
	// A failed food switch must leave the previous snapshot intact.
	e.Set("food_name", "Missing food")
	doForm(t, h, "POST", "/nutrition/entries/1", e, true, 400)
	after, err := a.listFoodEntries(context.Background(), 10)
	if err != nil || !reflect.DeepEqual(entries, after) {
		t.Fatalf("invalid selection changed history: %+v %v", after, err)
	}
	// The remaining library food must not silently replace a deleted source.
	doForm(t, h, "POST", "/nutrition/foods/2/delete", url.Values{"csrf": {"csrf"}}, true, 303)
	w := request(h, "GET", "/nutrition", "", "test")
	if !strings.Contains(w.Body.String(), `name="food_name" value="Other food"`) {
		t.Fatal("deleted source lacks its saved food name")
	}
	e.Set("food_name", "Other food")
	e.Set("consumed_g", "200")
	logReviewedFood(t, a, "/nutrition/entries/1", e)
	if got := mustDailyNutrition(t, a.db, "2026-01-01"); got.Carbohydrate != 40 {
		t.Fatalf("orphaned snapshot changed: %+v", got)
	}
}

func TestReadHelpersPropagateFailures(t *testing.T) {
	a := testApp(t)
	if err := a.db.Close(); err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	_, totalsErr := dailyNutrition(ctx, a.db, "2026-01-01")
	_, _, _, _, _, _, _, _, nutritionErr := a.nutritionSeries(ctx, 7)
	_, _, _, _, bodyErr := a.bodySeries(ctx, 7, profile{})
	_, _, sleepErr := a.sleepSeries(ctx, 7)
	for name, err := range map[string]error{"totals": totalsErr, "nutrition": nutritionErr, "body": bodyErr, "sleep": sleepErr} {
		if err == nil {
			t.Errorf("%s hid a read failure", name)
		}
	}
}

func TestCalendarSleepBoundaries(t *testing.T) {
	for _, tc := range []struct {
		zone, day, bed, wake string
		want                 float64
		invalid              bool
	}{
		{"Europe/Madrid", "2026-03-29", "02:30", "07:00", 0, true},
		{"Europe/Madrid", "2026-03-29", "23:00", "02:30", 0, true},
		{"Australia/Lord_Howe", "2026-04-05", "23:00", "07:00", 8.5, false},
		{"Australia/Lord_Howe", "2026-10-04", "23:00", "07:00", 7.5, false},
		{"UTC", "2024-03-01", "23:59", "00:00", 1.0 / 60, false},
		{"UTC", "2026-01-01", "07:00", "07:00", 24, false},
		{"Europe/Madrid", "2026-10-25", "07:00", "07:00", 0, true}, // Existing 24-hour storage limit.
	} {
		t.Run(tc.zone+"/"+tc.day+"/"+tc.bed+"/"+tc.wake, func(t *testing.T) {
			loc, err := time.LoadLocation(tc.zone)
			if err != nil {
				t.Fatal(err)
			}
			got, err := sleepDurationOn(tc.day, tc.bed, tc.wake, loc)
			if (err != nil) != tc.invalid || !tc.invalid && got != tc.want {
				t.Fatalf("got %g (%v), want %g invalid=%t", got, err, tc.want, tc.invalid)
			}
		})
	}
}

func TestNameLimitMatchesUTF16(t *testing.T) {
	for _, tc := range []struct {
		name string
		ok   bool
	}{
		{strings.Repeat("é", 120), true},
		{strings.Repeat("é", 121), false},
		{strings.Repeat("🍎", 60), true},
		{strings.Repeat("🍎", 61), false},
		{"invalid\xff", false},
	} {
		if got := validName(tc.name); got != tc.ok {
			t.Errorf("validName(%q) = %t, want %t", tc.name, got, tc.ok)
		}
	}
}

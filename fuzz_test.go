package main

import (
	"fmt"
	"math"
	"net/http/httptest"
	"net/url"
	"strconv"
	"strings"
	"testing"
	"time"
)

func FuzzFormFloat(f *testing.F) {
	for _, s := range []string{"0", "-0", "100", "100.00000000000001", "-0.0001", "NaN", "Inf", "-Inf", "1e9999", "", " 1", "0x1p2", "1_0", "1\x00"} {
		f.Add(s)
	}
	f.Fuzz(func(t *testing.T, s string) {
		r := httptest.NewRequest("POST", "/", strings.NewReader(url.Values{"n": {s}}.Encode()))
		r.Header.Set("Content-Type", "application/x-www-form-urlencoded")
		got, err := formFloat(r, "n", 0, 100)
		if err == nil {
			if math.IsNaN(got) || math.IsInf(got, 0) || got < 0 || got > 100 {
				t.Fatalf("accepted invalid numeric value %q -> %g", s, got)
			}
			// A returned number must survive a decimal round trip unchanged.
			back, e := strconv.ParseFloat(strconv.FormatFloat(got, 'g', -1, 64), 64)
			if e != nil || back != got {
				t.Fatalf("unstable float %q -> %g", s, got)
			}
		}
		opt, optErr := optionalFloat(r, "n", 0, 100)
		if strings.TrimSpace(s) == "" {
			if optErr != nil || opt != nil {
				t.Fatal("blank optional value is not absent")
			}
		} else if (err == nil) != (optErr == nil) || (optErr == nil && (opt == nil || *opt != got)) {
			t.Fatal("required and optional validation disagree")
		}
	})
}

func FuzzValidDate(f *testing.F) {
	for _, s := range []string{"2024-02-29", "1900-02-29", "2000-02-29", "2026-01-01", "2026-01-1", "2026-1-01", "2026-04-31", "0000-01-01", "", "2026-01-01\x00"} {
		f.Add(s)
	}
	f.Fuzz(func(t *testing.T, s string) {
		if !validDate(s) {
			return
		}
		// Dates are stored as text and matched against ISO chart labels.
		parsed, err := time.Parse("2006-01-02", s)
		if err != nil || parsed.Year() < 1 || parsed.Format("2006-01-02") != s {
			t.Fatalf("accepted date %q cannot round-trip through input type=date", s)
		}
	})
}

func FuzzSleepClockArithmetic(f *testing.F) {
	for _, pair := range [][2]uint16{{0, 0}, {0, 1}, {1439, 0}, {1410, 420}, {60, 59}} {
		f.Add(pair[0], pair[1])
	}
	f.Fuzz(func(t *testing.T, bed, wake uint16) {
		b, w := int(bed)%1440, int(wake)%1440
		got, err := sleepDuration(fmt.Sprintf("%02d:%02d", b/60, b%60), fmt.Sprintf("%02d:%02d", w/60, w%60))
		minutes := (w - b + 1440) % 1440
		if minutes == 0 {
			minutes = 1440
		} // Existing convention: equal clocks mean 24h.
		if err != nil || math.Abs(got*60-float64(minutes)) > 1e-9 {
			t.Fatalf("bed=%d wake=%d: got %g %v, want %d minutes", b, w, got, err, minutes)
		}
	})
}

func FuzzSleepMalformed(f *testing.F) {
	for _, s := range []string{"23:00", "00:00", "24:00", "23:60", "1:00", "-1:00", "12:00:00", "12:00Z", "NaN", ""} {
		f.Add(s)
	}
	f.Fuzz(func(t *testing.T, s string) {
		for _, pair := range [][2]string{{s, "07:00"}, {"23:00", s}} {
			got, err := sleepDuration(pair[0], pair[1])
			if err == nil && (math.IsNaN(got) || got <= 0 || got > 24) {
				t.Fatalf("invalid duration for %q: %g", pair, got)
			}
		}
	})
}

func FuzzDateLabels(f *testing.F) {
	for _, days := range []uint16{1, 7, 30, 90, 365} {
		f.Add(int64(1774746000), days, uint8(0))
	}
	f.Fuzz(func(t *testing.T, seconds int64, count uint16, zone uint8) {
		// Bound to years 2000–2100 while covering leap years and DST.
		seconds = int64(uint64(seconds)%3155760000) + 946684800
		zones := []string{"UTC", "Europe/Madrid", "America/New_York", "Australia/Lord_Howe"}
		loc, err := time.LoadLocation(zones[int(zone)%len(zones)])
		if err != nil {
			t.Fatal(err)
		}
		now := time.Unix(seconds, 0).In(loc)
		n := int(count)%365 + 1
		labels := dateLabels(now, n)
		if len(labels) != n || labels[n-1] != now.Format("2006-01-02") {
			t.Fatal("wrong length or end date")
		}
		// UTC date arithmetic is independent of DST shifts in the source zone.
		end := time.Date(now.Year(), now.Month(), now.Day(), 0, 0, 0, 0, time.UTC)
		for i, label := range labels {
			want := end.Add(time.Duration(i-n+1) * 24 * time.Hour).Format("2006-01-02")
			if label != want {
				t.Fatalf("%s at %s: label %d = %s, want %s", loc, now, i, label, want)
			}
		}
	})
}

func FuzzIDAndRange(f *testing.F) {
	for _, s := range []string{"", "0", "-1", "1", "9223372036854775807", "9223372036854775808", "7", "30", "90", "365", "../../", "1 OR 1=1"} {
		f.Add(s)
	}
	f.Fuzz(func(t *testing.T, s string) {
		r := httptest.NewRequest("GET", "/?"+url.Values{"days": {s}}.Encode(), nil)
		r.SetPathValue("id", s)
		id, err := parseID(r)
		if err == nil && ((s == "" && id != 0) || (s != "" && id <= 0)) {
			t.Fatalf("invalid ID %q -> %d", s, id)
		}
		days := parseRange(r)
		if days != 7 && days != 30 && days != 90 && days != 365 {
			t.Fatalf("unbounded chart range %q -> %d", s, days)
		}
	})
}

func FuzzFoodWriteValidation(f *testing.F) {
	for _, values := range [][4]float64{{0, 0, 0, 0}, {100, 100, 100, 100}, {60, 10, 0, 12}, {1, 2, 0, 0}, {1, 1, 2, 0}, {math.NaN(), 0, 0, 0}, {0, 0, 0, math.Inf(1)}} {
		f.Add(values[0], values[1], values[2], values[3])
	}
	f.Fuzz(func(t *testing.T, carb, sugar, fat, protein float64) {
		v := foodForm()
		for k, n := range map[string]float64{"carbohydrate": carb, "total_sugar": sugar, "fat": fat, "protein": protein} {
			v.Set(k, strconv.FormatFloat(n, 'g', -1, 64))
		}
		valid := true
		for _, n := range []float64{carb, sugar, fat, protein} {
			valid = valid && !math.IsNaN(n) && !math.IsInf(n, 0) && n >= 0 && n <= 100
		}
		valid = valid && sugar <= carb
		got, err := fixtureNutrition(v)
		if (err == nil) != valid {
			t.Fatalf("nutrients=%v error=%v want accepted=%t", []float64{carb, sugar, fat, protein}, err, valid)
		}
		if valid && (got.Carbohydrate != carb || got.TotalSugar != sugar || got.Fat == nil || *got.Fat != fat || got.Protein != protein) {
			t.Fatalf("decoded nutrients changed: %+v", got)
		}
	})
}

package main

import (
	"context"
	"database/sql"
	"fmt"
	"math"
	"math/rand"
	"net/http"
	"net/http/httptest"
	"net/url"
	"path/filepath"
	"reflect"
	"strings"
	"sync"
	"testing"
	"time"
)

// These tests assert the contracts found during implementation validation.
func mustDailyNutrition(t *testing.T, db *sql.DB, date string) nutritionTotals {
	t.Helper()
	value, err := dailyNutrition(context.Background(), db, date)
	if err != nil {
		t.Fatal(err)
	}
	return value
}

func request(h http.Handler, method, path, body, token string) *httptest.ResponseRecorder {
	r := httptest.NewRequest(method, path, strings.NewReader(body))
	r.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	if token != "" {
		r.AddCookie(&http.Cookie{Name: "session", Value: token})
	}
	w := httptest.NewRecorder()
	h.ServeHTTP(w, r)
	return w
}

func foodForm() url.Values {
	return url.Values{"csrf": {"csrf"}, "name": {"Oats"}, "carbohydrate": {"60"}, "total_sugar": {"10"}, "free_sugar_percent": {"0"}, "protein": {"12"}, "fat": {"7"}, "fiber": {"10"}, "salt": {"0"}}
}

func entryForm() url.Values {
	return url.Values{"csrf": {"csrf"}, "entry_date": {"2026-01-01"}, "food_name": {"Oats"}, "consumed_g": {"100"}}
}

func readingForm(kind, date string) url.Values {
	v := url.Values{"csrf": {"csrf"}, "entry_date": {date}}
	if kind == "body" {
		v.Set("weight_kg", "80")
		v.Set("waist_cm", "90")
	} else {
		v.Set("bed_time", "23:00")
		v.Set("wake_time", "07:00")
	}
	return v
}

func execSQL(t *testing.T, db *sql.DB, query string, args ...any) {
	t.Helper()
	if _, err := db.Exec(query, args...); err != nil {
		t.Fatal(err)
	}
}

func TestValidationMalformedFormsAreAtomic(t *testing.T) {
	for _, path := range []string{"/body", "/sleep", "/nutrition/foods", "/settings"} {
		t.Run(path, func(t *testing.T) {
			a := testApp(t)
			v := readingForm(strings.TrimPrefix(path, "/"), "2026-01-01")
			table := strings.TrimPrefix(path, "/") + "_entries"
			if path == "/nutrition/foods" {
				v, table = foodForm(), "foods"
			} else if path == "/settings" {
				v = url.Values{"csrf": {"csrf"}, "name": {"Changed"}, "energy_target": {"2500"}}
				table = "profile WHERE name='Changed'"
			}
			// One invalid escape is enough: FormValue consumes ParseForm's error.
			w := request(a.routes(), "POST", path, v.Encode()+"&bad=%", "test")
			var n int
			if err := a.db.QueryRow("SELECT COUNT(*) FROM " + table).Scan(&n); err != nil {
				t.Fatal(err)
			}
			if w.Code < 400 || n != 0 {
				t.Errorf("malformed form returned %d and changed %d rows; want rejection and no change", w.Code, n)
			}
		})
	}
}

func TestValidationOversizedQueryFallback(t *testing.T) {
	a := testApp(t)
	w := request(a.routes(), "POST", "/body?"+readingForm("body", "2026-01-01").Encode(), strings.Repeat("x", (1<<20)+1), "test")
	var n int
	if err := a.db.QueryRow("SELECT COUNT(*) FROM body_entries").Scan(&n); err != nil {
		t.Fatal(err)
	}
	if w.Code < 400 || n != 0 {
		t.Fatalf("oversized body with valid query: status=%d rows=%d", w.Code, n)
	}
}

func TestValidationReadingIdentityAndRetry(t *testing.T) {
	for _, kind := range []string{"body", "sleep"} {
		t.Run(kind, func(t *testing.T) {
			a := testApp(t)
			h := a.routes()
			for _, date := range []string{"2026-01-01", "2026-01-02"} {
				doForm(t, h, "POST", "/"+kind, readingForm(kind, date), true, 303)
			}
			edit := readingForm(kind, "2026-01-03")
			doForm(t, h, "POST", "/"+kind+"/1", edit, true, 303)
			var id int64
			if err := a.db.QueryRow("SELECT id FROM " + kind + "_entries WHERE entry_date='2026-01-03'").Scan(&id); err != nil {
				t.Fatal(err)
			}
			if id != 1 {
				t.Errorf("editing record 1 changed its ID to %d; stale forms now target a missing record", id)
			}
			// Retry after an uncertain response, then delete using the original link.
			doForm(t, h, "POST", "/"+kind+"/1", edit, true, 303)
			w := request(h, "POST", "/"+kind+"/1/delete", "csrf=csrf", "test")
			if w.Code != 303 {
				t.Errorf("delete original ID after edit/retry: %d", w.Code)
			}
		})
	}
}

func TestValidationMissingUpdateDoesNotSucceed(t *testing.T) {
	for _, path := range []string{"/nutrition/foods/99", "/nutrition/entries/99", "/body/99", "/sleep/99"} {
		t.Run(path, func(t *testing.T) {
			a := testApp(t)
			h := a.routes()
			seedFood(t, a, foodForm(), 0)
			v := foodForm()
			if strings.Contains(path, "entries") {
				v = entryForm()
			} else if strings.HasPrefix(path, "/body") {
				v = readingForm("body", "2026-01-01")
			} else if strings.HasPrefix(path, "/sleep") {
				v = readingForm("sleep", "2026-01-01")
			}
			w := request(h, "POST", path, v.Encode(), "test")
			if w.Code < 400 {
				t.Errorf("update of missing ID returned success: %d", w.Code)
			}
			for _, table := range []string{"body_entries", "sleep_entries", "food_entries"} {
				var n int
				if err := a.db.QueryRow("SELECT COUNT(*) FROM " + table).Scan(&n); err != nil {
					t.Fatal(err)
				}
				if n != 0 {
					t.Errorf("missing update created %d records in %s", n, table)
				}
			}
		})
	}
}

func TestValidationAmountEditUsesSavedSnapshot(t *testing.T) {
	a := testApp(t)
	seedFood(t, a, foodForm(), 0)
	logReviewedFood(t, a, "/nutrition/entries", entryForm())
	v := foodForm()
	v.Set("carbohydrate", "30")
	seedFood(t, a, v, 1)
	e := entryForm()
	e.Set("consumed_g", "200")
	logReviewedFood(t, a, "/nutrition/entries/1", e)
	got := mustDailyNutrition(t, a.db, "2026-01-01")
	if got.Carbohydrate != 120 || got.Fat == nil || *got.Fat != 14 {
		t.Fatalf("amount edit did not use saved nutrients: %+v", got)
	}
}

func TestValidationReadFailuresAreNotEmptyData(t *testing.T) {
	for _, path := range []string{"/", "/nutrition", "/body", "/sleep", "/settings"} {
		t.Run(path, func(t *testing.T) {
			a := testApp(t)
			if err := a.db.Close(); err != nil {
				t.Fatal(err)
			}
			w := request(a.routes(), "GET", path, "", "test")
			if w.Code < 500 {
				t.Fatalf("unavailable DB rendered success (%d), concealing missing data", w.Code)
			}
		})
	}
}

func TestValidationSleepElapsedAcrossDST(t *testing.T) {
	loc, err := time.LoadLocation("Europe/Madrid")
	if err != nil {
		t.Fatal(err)
	}
	for _, date := range []string{"2026-03-29", "2026-10-25"} {
		t.Run(date, func(t *testing.T) {
			a := testApp(t)
			a.location = loc
			wake, err := time.ParseInLocation("2006-01-02 15:04", date+" 07:00", loc)
			if err != nil {
				t.Fatal(err)
			}
			previous := wake.AddDate(0, 0, -1)
			bed := time.Date(previous.Year(), previous.Month(), previous.Day(), 23, 0, 0, 0, loc)
			want := wake.Sub(bed).Hours()
			doForm(t, a.routes(), "POST", "/sleep", readingForm("sleep", date), true, 303)
			var got float64
			if err := a.db.QueryRow("SELECT duration_hours FROM sleep_entries").Scan(&got); err != nil {
				t.Fatal(err)
			}
			if got != want {
				t.Errorf("23:00–07:00 ending %s: got %g hours, actual elapsed %g", date, got, want)
			}
		})
	}
}

func TestValidationReadingRollbackOnUpdateFailure(t *testing.T) {
	for _, kind := range []string{"body", "sleep"} {
		t.Run(kind, func(t *testing.T) {
			a := testApp(t)
			h := a.routes()
			doForm(t, h, "POST", "/"+kind, readingForm(kind, "2026-01-01"), true, 303)
			execSQL(t, a.db, "CREATE TRIGGER fail_update BEFORE UPDATE ON "+kind+"_entries BEGIN SELECT RAISE(ABORT, 'injected storage failure'); END")
			w := request(h, "POST", "/"+kind+"/1", readingForm(kind, "2026-01-02").Encode(), "test")
			if w.Code < 400 {
				t.Fatalf("injected failure returned %d", w.Code)
			}
			var date string
			if err := a.db.QueryRow("SELECT entry_date FROM " + kind + "_entries WHERE id=1").Scan(&date); err != nil || date != "2026-01-01" {
				t.Fatalf("failed edit lost original: date=%s err=%v", date, err)
			}
		})
	}
}

func TestValidationMigrationRetryAndRollback(t *testing.T) {
	a := testApp(t)
	execSQL(t, a.db, "UPDATE profile SET name='Keep me'")
	for range 3 {
		if err := migrate(a.db); err != nil {
			t.Fatal(err)
		}
	}
	p, err := a.getProfile(context.Background())
	if err != nil || p.Name != "Keep me" {
		t.Fatalf("migration replay changed data: %+v %v", p, err)
	}
	db, err := sql.Open("sqlite", filepath.Join(t.TempDir(), "broken.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	execSQL(t, db, "CREATE TABLE foods(id INTEGER PRIMARY KEY)")
	if err := migrate(db); err == nil {
		t.Fatal("conflicting schema did not fail migration")
	}
	var n int
	if err := db.QueryRow("SELECT COUNT(*) FROM sqlite_master WHERE name='profile'").Scan(&n); err != nil || n != 0 {
		t.Fatalf("migration left partial schema: %d %v", n, err)
	}
	if err := db.QueryRow("SELECT COUNT(*) FROM schema_migrations").Scan(&n); err != nil || n != 0 {
		t.Fatalf("failed migration recorded as applied: %d %v", n, err)
	}
	execSQL(t, db, "DROP TABLE foods")
	if err := migrate(db); err != nil {
		t.Fatalf("migration could not retry after rollback: %v", err)
	}
}

func TestValidationNutritionRandomizedModel(t *testing.T) {
	// Independent in-memory oracle, exact binary fractions, repeatable seed.
	for _, seed := range []int64{1, 17, 20260927} {
		t.Run(fmt.Sprint(seed), func(t *testing.T) {
			a := testApp(t)
			rng := rand.New(rand.NewSource(seed))
			now := time.Now().In(a.location)
			type total struct {
				c, s, fs, p, fat, fiber, salt float64
				count, unknown                int
			}
			model := map[string]total{}
			for i := range 150 {
				date := now.AddDate(0, 0, rng.Intn(12)-9).Format("2006-01-02")
				c, s, freePercent, p, fa, fiber, salt := float64(rng.Intn(101)), float64(rng.Intn(11)), float64(25*rng.Intn(5)), float64(rng.Intn(101)), float64(rng.Intn(101)), float64(rng.Intn(101)), float64(rng.Intn(101))
				s = math.Min(s, c)
				grams := float64(25 * (1 + rng.Intn(8)))
				var fat any
				m := model[date]
				if i%7 == 0 {
					m.unknown++
				} else {
					fat = fa
					m.fat += fa * grams / 100
				}
				execSQL(t, a.db, `INSERT INTO food_entries(entry_date,consumed_g,food_name,carbohydrate,total_sugar,free_sugar_percent,protein,fat,fiber,salt) VALUES(?,?,'Snapshot',?,?,?,?,?,?,?)`, date, grams, c, s, freePercent, p, fat, fiber, salt)
				m.c += c * grams / 100
				m.s += s * grams / 100
				m.fs += s * freePercent * grams / 10000
				m.p += p * grams / 100
				m.fiber += fiber * grams / 100
				m.salt += salt * grams / 100
				m.count++
				model[date] = m
			}
			labels, c, totalSugar, freeSugar, p, fat, f, salt, err := a.nutritionSeries(context.Background(), 7)
			if err != nil {
				t.Fatal(err)
			}
			for i, date := range labels {
				m := model[date]
				d := mustDailyNutrition(t, a.db, date)
				got := []float64{c[i], totalSugar[i], freeSugar[i], p[i], f[i], salt[i], d.Carbohydrate, d.TotalSugar, d.FreeSugar, d.Protein, d.Fiber, d.Salt}
				want := []float64{m.c, m.s, m.fs, m.p, m.fiber, m.salt, m.c, m.s, m.fs, m.p, m.fiber, m.salt}
				if !reflect.DeepEqual(got, want) {
					t.Fatalf("date %s: got %v want %v", date, got, want)
				}
				wantKnown := m.count > 0 && m.unknown == 0
				if (d.Fat != nil) != wantKnown {
					t.Fatalf("unknown propagation failed for %s", date)
				}
				if wantKnown && (fat[i] != m.fat || *d.Fat != m.fat) {
					t.Fatalf("fat scaling failed for %s", date)
				}
				if m.count == 0 && fat[i] != 0.0 {
					t.Fatalf("empty day fat = %v, want zero", fat[i])
				}
				if m.unknown > 0 && fat[i] != nil {
					t.Fatalf("unknown fat shown as %v", fat[i])
				}
			}
		})
	}
}

func TestValidationFoodDeletionPreservesHistory(t *testing.T) {
	a := testApp(t)
	h := a.routes()
	seedFood(t, a, foodForm(), 0)
	logReviewedFood(t, a, "/nutrition/entries", entryForm())
	before := mustDailyNutrition(t, a.db, "2026-01-01")
	doForm(t, h, "POST", "/nutrition/foods/1/delete", url.Values{"csrf": {"csrf"}}, true, 303)
	after := mustDailyNutrition(t, a.db, "2026-01-01")
	if !reflect.DeepEqual(before, after) {
		t.Fatalf("deleting food changed history: %+v -> %+v", before, after)
	}
	entries, err := a.listFoodEntries(context.Background(), 10)
	if err != nil || len(entries) != 1 || entries[0].FoodID != 0 || entries[0].FoodName != "Oats" {
		t.Fatalf("history after deletion: %+v %v", entries, err)
	}
	// Reusing the old food ID must not reattach its historical entries.
	v := foodForm()
	v.Set("name", "New food")
	seedFood(t, a, v, 0)
	entries, err = a.listFoodEntries(context.Background(), 10)
	if err != nil || entries[0].FoodID != 0 {
		t.Fatalf("history reattached: %+v %v", entries, err)
	}
}

func TestValidationConcurrentDailyReplacement(t *testing.T) {
	for _, kind := range []string{"body", "sleep"} {
		t.Run(kind, func(t *testing.T) {
			a := testApp(t)
			h := a.routes()
			start := make(chan struct{})
			results := make(chan int, 24)
			for i := range 24 {
				go func(i int) {
					<-start
					v := readingForm(kind, "2026-01-01")
					if kind == "body" {
						v.Set("weight_kg", fmt.Sprint(50+i))
						v.Set("waist_cm", fmt.Sprint(100+i))
					} else {
						v.Set("bed_time", fmt.Sprintf("%02d:00", i))
					}
					results <- request(h, "POST", "/"+kind, v.Encode(), "test").Code
				}(i)
			}
			close(start)
			for range 24 {
				if code := <-results; code != 303 {
					t.Errorf("concurrent write returned %d", code)
				}
			}
			var n int
			if err := a.db.QueryRow("SELECT COUNT(*) FROM " + kind + "_entries").Scan(&n); err != nil || n != 1 {
				t.Fatalf("count=%d err=%v", n, err)
			}
			if kind == "body" {
				var w, waist float64
				if err := a.db.QueryRow("SELECT weight_kg,waist_cm FROM body_entries").Scan(&w, &waist); err != nil || waist-w != 50 {
					t.Fatalf("torn write: %g %g %v", w, waist, err)
				}
			} else {
				var bed, wake string
				var duration float64
				if err := a.db.QueryRow("SELECT bed_time,wake_time,duration_hours FROM sleep_entries").Scan(&bed, &wake, &duration); err != nil {
					t.Fatal(err)
				}
				want, err := sleepDuration(bed, wake)
				if err != nil || duration != want {
					t.Fatalf("torn sleep write: %s %s %g %v", bed, wake, duration, err)
				}
			}
		})
	}
}

func TestValidationSessionLifecycleConcurrent(t *testing.T) {
	a := testApp(t)
	a.cfg.secureCookie = true
	h := a.routes()
	login := url.Values{"email": {a.cfg.email}, "password": {a.cfg.password}}.Encode()
	w := request(h, "POST", "/login", login, "")
	if w.Code != 303 {
		t.Fatalf("login: %d", w.Code)
	}
	cookies := w.Result().Cookies()
	if len(cookies) != 1 {
		t.Fatalf("cookies=%v", cookies)
	}
	c := cookies[0]
	if len(c.Value) != 64 || !c.HttpOnly || !c.Secure || c.SameSite != http.SameSiteStrictMode || c.Path != "/" || c.MaxAge != 43200 {
		t.Fatalf("unsafe cookie: %+v", c)
	}
	a.sessions.Lock()
	csrf := a.sessions.m[c.Value].csrf
	a.sessions.Unlock()
	if code := request(h, "POST", "/logout", "csrf=csrf", c.Value).Code; code != 403 {
		t.Fatalf("cross-session token accepted: %d", code)
	}
	var wg sync.WaitGroup
	for range 12 {
		wg.Go(func() {
			request(h, "GET", "/settings", "", c.Value)
			request(h, "POST", "/logout", "csrf="+csrf, c.Value)
		})
	}
	wg.Wait()
	if code := request(h, "GET", "/settings", "", c.Value).Code; code != 303 {
		t.Fatalf("logged-out token still works: %d", code)
	}
	a.sessions.Lock()
	a.sessions.m["expired"] = session{csrf: "csrf", expires: time.Now().Add(-time.Second)}
	a.sessions.Unlock()
	if code := request(h, "POST", "/body", readingForm("body", "2026-01-01").Encode(), "expired").Code; code != 303 {
		t.Fatalf("expired session: %d", code)
	}
	a.sessions.Lock()
	_, exists := a.sessions.m["expired"]
	a.sessions.Unlock()
	if exists {
		t.Fatal("expired session not removed")
	}
}

func TestValidationCanceledWrite(t *testing.T) {
	a := testApp(t)
	r := httptest.NewRequest("POST", "/body", strings.NewReader(readingForm("body", "2026-01-01").Encode()))
	r.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	r.AddCookie(&http.Cookie{Name: "session", Value: "test"})
	ctx, cancel := context.WithCancel(r.Context())
	cancel()
	w := httptest.NewRecorder()
	a.routes().ServeHTTP(w, r.WithContext(ctx))
	var n int
	if err := a.db.QueryRow("SELECT COUNT(*) FROM body_entries").Scan(&n); err != nil {
		t.Fatal(err)
	}
	if n != 0 {
		t.Fatalf("already-canceled request committed %d writes (status %d)", n, w.Code)
	}
}

func TestValidationDeadlineWhileWaitingForDatabase(t *testing.T) {
	a := testApp(t)
	conn, err := a.db.Conn(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Millisecond)
	defer cancel()
	r := httptest.NewRequest("POST", "/body", strings.NewReader(readingForm("body", "2026-01-01").Encode())).WithContext(ctx)
	r.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	r.AddCookie(&http.Cookie{Name: "session", Value: "test"})
	done := make(chan int, 1)
	go func() { w := httptest.NewRecorder(); a.routes().ServeHTTP(w, r); done <- w.Code }()
	<-ctx.Done()
	select {
	case <-done:
	case <-time.After(200 * time.Millisecond):
		t.Error("request still waiting for the only DB connection 200ms after deadline")
		conn.Close()
		select {
		case <-done:
		case <-time.After(2 * time.Second):
			t.Fatal("handler did not finish after releasing connection")
		}
	}
	conn.Close()
	var n int
	if err := a.db.QueryRow("SELECT COUNT(*) FROM body_entries").Scan(&n); err != nil {
		t.Fatal(err)
	}
	if n != 0 {
		t.Errorf("request committed %d records after its deadline", n)
	}
}

func TestValidationBoundaryWrites(t *testing.T) {
	for _, tc := range []struct {
		field, value string
		ok           bool
	}{
		{"carbohydrate", "0", false}, // Existing sugar=10 makes this invalid.
		{"carbohydrate", "10", true}, {"carbohydrate", "100", true}, {"carbohydrate", "100.00000000000001", false},
		{"total_sugar", "60", true}, {"total_sugar", "60.00000000000001", false},
		{"free_sugar_percent", "0", true}, {"free_sugar_percent", "100", true}, {"free_sugar_percent", "100.01", false},
		{"protein", "-0", true}, {"protein", "-0.000001", false}, {"protein", "NaN", false}, {"protein", "+Inf", false},
		{"fat", "0", true}, {"fat", "100", true}, {"fat", "100.01", false}, {"fat", "", false},
		{"fiber", "100", true}, {"fiber", "1e9999", false}, {"salt", "0", true}, {"salt", "-Inf", false},
		{"name", " ", false}, {"name", strings.Repeat("a", 2000), true}, {"name", strings.Repeat("a", 2001), false},
	} {
		t.Run(tc.field+"="+tc.value, func(t *testing.T) {
			v := foodForm()
			v.Set(tc.field, tc.value)
			_, err := fixtureNutrition(v)
			accepted := err == nil && validDescription(strings.TrimSpace(v.Get("name")))
			if accepted != tc.ok {
				t.Fatalf("accepted=%t error=%v want %t", accepted, err, tc.ok)
			}
		})
	}
}

func TestValidationUnauthorizedMutations(t *testing.T) {
	a := testApp(t)
	h := a.routes()
	for _, path := range []string{"/body", "/body/1", "/body/1/delete", "/sleep", "/sleep/1", "/sleep/1/delete", "/nutrition/foods", "/nutrition/foods/1", "/nutrition/foods/1/delete", "/nutrition/entries", "/nutrition/entries/1", "/nutrition/entries/1/delete", "/settings", "/logout"} {
		for _, token := range []string{"", "unknown", "test"} {
			w := request(h, "POST", path, "csrf=wrong", token)
			want := 303
			if token == "test" {
				want = 403
			}
			if w.Code != want {
				t.Errorf("%s cookie=%q: %d want %d", path, token, w.Code, want)
			}
		}
	}
}

func TestValidationCanonicalDateStorage(t *testing.T) {
	a := testApp(t)
	h := a.routes()
	doForm(t, h, "POST", "/body", readingForm("body", "2026-01-01"), true, 303)
	w := request(h, "POST", "/body", readingForm("body", "2026-01-1").Encode(), "test")
	var n int
	if err := a.db.QueryRow("SELECT COUNT(*) FROM body_entries").Scan(&n); err != nil {
		t.Fatal(err)
	}
	if w.Code < 400 || n != 1 {
		t.Fatalf("noncanonical spelling of same day: status=%d rows=%d; breaks one-reading-per-day", w.Code, n)
	}
}

func TestValidationBodyAndSleepChartWindows(t *testing.T) {
	a := testApp(t)
	now := time.Now().In(a.location)
	for _, offset := range []int{-7, -6, 0, 1} {
		date := now.AddDate(0, 0, offset).Format("2006-01-02")
		execSQL(t, a.db, "INSERT INTO body_entries(entry_date,weight_kg,waist_cm) VALUES(?,80,90)", date)
		execSQL(t, a.db, "INSERT INTO sleep_entries(entry_date,bed_time,wake_time,duration_hours) VALUES(?,'23:00','07:00',8)", date)
	}
	height := 200.0
	for _, h := range []*float64{nil, &height} {
		labels, weights, bmis, waists, err := a.bodySeries(context.Background(), 7, profile{HeightCM: h})
		if err != nil {
			t.Fatal(err)
		}
		sleepLabels, hours, err := a.sleepSeries(context.Background(), 7)
		if err != nil {
			t.Fatal(err)
		}
		if !reflect.DeepEqual(labels, sleepLabels) || len(labels) != 7 {
			t.Fatal("chart calendars differ")
		}
		for i := range labels {
			if i == 0 || i == 6 {
				if weights[i] != 80.0 || waists[i] != 90.0 || hours[i] != 8.0 {
					t.Fatalf("boundary %d missing: %v %v %v", i, weights[i], waists[i], hours[i])
				}
				if h == nil && bmis[i] != nil || h != nil && bmis[i] != 20.0 {
					t.Fatalf("height presence not respected: %v", bmis[i])
				}
			} else if weights[i] != nil || waists[i] != nil || hours[i] != nil || bmis[i] != nil {
				t.Fatalf("missing day %d replaced with values", i)
			}
		}
	}
}

func TestValidationUnicodeNameMatchesFormLimit(t *testing.T) {
	// 61 BMP characters fit the browser's maxlength=120; they occupy 122 UTF-8 bytes.
	for _, path := range []string{"/settings"} {
		t.Run(path, func(t *testing.T) {
			a := testApp(t)
			v := foodForm()
			v.Set("name", strings.Repeat("é", 61))
			v.Set("energy_target", "2000")
			w := request(a.routes(), "POST", path, v.Encode(), "test")
			if w.Code != 303 {
				t.Fatalf("browser-valid 61-character name rejected: %d %s", w.Code, w.Body.String())
			}
		})
	}
}

func TestValidationDeletedFoodEntryRemainsEditable(t *testing.T) {
	a := testApp(t)
	h := a.routes()
	seedFood(t, a, foodForm(), 0)
	logReviewedFood(t, a, "/nutrition/entries", entryForm())
	doForm(t, h, "POST", "/nutrition/foods/1/delete", url.Values{"csrf": {"csrf"}}, true, 303)
	// The saved name still identifies the retained snapshot after deletion.
	v := entryForm()
	v.Set("consumed_g", "200")
	w := request(h, "POST", "/nutrition/entries/1", v.Encode(), "test")
	if w.Code != 303 {
		t.Fatalf("cannot edit retained snapshot after source deletion: %d %s", w.Code, w.Body.String())
	}
}

func TestValidationSleepTimeRoundTrip(t *testing.T) {
	a := testApp(t)
	v := readingForm("sleep", "2026-01-01")
	v.Set("bed_time", "1:00")
	w := request(a.routes(), "POST", "/sleep", v.Encode(), "test")
	if w.Code >= 400 {
		return
	} // Rejection or normalization both preserve the HTML time contract.
	var bed string
	if err := a.db.QueryRow("SELECT bed_time FROM sleep_entries").Scan(&bed); err != nil {
		t.Fatal(err)
	}
	parsed, err := time.Parse("15:04", bed)
	if err != nil || parsed.Format("15:04") != bed {
		t.Fatalf("accepted clock %q cannot round-trip through input type=time", bed)
	}
}

func TestValidationNumericFormBoundaries(t *testing.T) {
	for _, tc := range []struct {
		path, field, value string
		ok                 bool
	}{
		{"/body", "weight_kg", "1", true}, {"/body", "weight_kg", "1000", true}, {"/body", "weight_kg", "0.999999", false}, {"/body", "waist_cm", "500.001", false},
		{"/nutrition/entries", "consumed_g", "0.01", true}, {"/nutrition/entries", "consumed_g", "100000", true}, {"/nutrition/entries", "consumed_g", "0.0099999", false}, {"/nutrition/entries", "consumed_g", "100000.00001", false},
		{"/nutrition/entries", "food_name", "Unknown food", false}, {"/nutrition/entries", "food_name", "", false},
		{"/settings", "height_cm", "", true}, {"/settings", "height_cm", "50", true}, {"/settings", "height_cm", "300", true}, {"/settings", "height_cm", "49.999", false},
		{"/settings", "energy_target", "500", true}, {"/settings", "energy_target", "10000", true}, {"/settings", "energy_target", "10000.01", false},
		{"/sleep", "bed_time", "24:00", false}, {"/sleep", "wake_time", "07:60", false}, {"/sleep", "entry_date", "2026-02-29", false},
	} {
		t.Run(tc.path+"/"+tc.field+"="+tc.value, func(t *testing.T) {
			a := testApp(t)
			h := a.routes()
			seedFood(t, a, foodForm(), 0)
			v := readingForm(strings.TrimPrefix(tc.path, "/"), "2026-01-01")
			if tc.path == "/nutrition/entries" {
				v = entryForm()
			}
			if tc.path == "/settings" {
				v = url.Values{"csrf": {"csrf"}, "energy_target": {"2000"}, "height_cm": {"180"}}
			}
			v.Set(tc.field, tc.value)
			path := tc.path
			want := 303
			if path == "/nutrition/entries" {
				path = "/nutrition/analyze"
				want = 200
			}
			w := request(h, "POST", path, v.Encode(), "test")
			if (w.Code == want) != tc.ok {
				t.Fatalf("status=%d body=%s want accepted=%t", w.Code, w.Body.String(), tc.ok)
			}
			if !tc.ok {
				for _, table := range []string{"body_entries", "sleep_entries", "food_entries"} {
					var n int
					if err := a.db.QueryRow("SELECT COUNT(*) FROM " + table).Scan(&n); err != nil {
						t.Fatal(err)
					}
					if n != 0 {
						t.Fatalf("rejected input changed %s", table)
					}
				}
				p, err := a.getProfile(context.Background())
				if err != nil || p.HeightCM != nil || p.EnergyTarget != 2000 {
					t.Fatalf("rejected input changed profile: %+v %v", p, err)
				}
			}
		})
	}
}

func TestValidationHTMLNamesAreEscaped(t *testing.T) {
	a := testApp(t)
	h := a.routes()
	name := `<script>alert(1)</script>`
	v := foodForm()
	v.Set("name", name)
	seedFood(t, a, v, 0)
	entry := entryForm()
	entry.Set("food_name", name)
	logReviewedFood(t, a, "/nutrition/entries", entry)
	for _, path := range []string{"/", "/nutrition"} {
		w := request(h, "GET", path, "", "test")
		if w.Code != 200 || strings.Contains(w.Body.String(), name) || !strings.Contains(w.Body.String(), "&lt;script&gt;") {
			t.Fatalf("unsafe or missing escaped name on %s", path)
		}
	}
}

package main

import (
	"database/sql"
	"encoding/json"
	"fmt"
	"html/template"
	"image/png"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func testApp(t *testing.T) *app {
	t.Helper()
	db, err := sql.Open("sqlite", filepath.Join(t.TempDir(), "test.db"))
	if err != nil {
		t.Fatal(err)
	}
	db.SetMaxOpenConns(1)
	if _, err = db.Exec("PRAGMA foreign_keys=ON; PRAGMA busy_timeout=5000; PRAGMA journal_mode=WAL;"); err != nil {
		t.Fatal(err)
	}
	if err = migrate(db); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { db.Close() })
	tpl, err := template.New("pages").Funcs(template.FuncMap{
		"f":   func(v float64) string { return fmt.Sprintf("%.1f", v) },
		"ptr": optionalNumber,
	}).ParseFS(files, "templates/*.html")
	if err != nil {
		t.Fatal(err)
	}
	a := &app{db: db, tpl: tpl, cfg: config{email: "me@example.com", password: "secret"}, version: "0.1.0", location: time.UTC}
	a.sessions.m = map[string]session{"test": {csrf: "csrf", expires: time.Now().Add(time.Hour)}}
	return a
}

func TestMigration(t *testing.T) {
	a := testApp(t)
	for _, table := range []string{"profile", "foods", "food_entries", "body_entries", "sleep_entries", "schema_migrations"} {
		var name string
		if err := a.db.QueryRow("SELECT name FROM sqlite_master WHERE type='table' AND name=?", table).Scan(&name); err != nil {
			t.Fatalf("table %s: %v", table, err)
		}
	}
}

func TestReferenceCalculations(t *testing.T) {
	if got := bmi(80, 200); got != 20 {
		t.Fatalf("bmi = %v", got)
	}
	cases := []struct {
		value float64
		want  string
	}{{18.49, "Below reference"}, {18.5, "Reference range"}, {25, "Above reference"}, {30, "High reference"}}
	for _, tc := range cases {
		if got := bmiBand(tc.value); got != tc.want {
			t.Errorf("bmiBand(%v) = %q", tc.value, got)
		}
	}
	if waistBand(85, 180) != "Below optimal range" || waistBand(86, 180) != "Optimal range" || waistBand(94, 180) != "Optimal range" || waistBand(95, 180) != "Above optimal range" {
		t.Fatal("waist boundaries are incorrect")
	}
	if got, err := sleepDuration("23:30", "07:00"); err != nil || got != 7.5 {
		t.Fatalf("overnight sleep = %v, %v", got, err)
	}
}

func TestNutritionScalingAndUnknownFreeSugar(t *testing.T) {
	a := testApp(t)
	_, err := a.db.Exec(`INSERT INTO foods(name,carbohydrate,total_sugar,free_sugar,protein,fiber,salt) VALUES('Known',50,10,5,20,8,1),('Unknown',20,5,NULL,10,2,0.5)`)
	if err != nil {
		t.Fatal(err)
	}
	_, err = a.db.Exec(`INSERT INTO food_entries(food_id,entry_date,consumed_g,food_name,carbohydrate,total_sugar,free_sugar,protein,fiber,salt) SELECT id,'2026-09-26',200,name,carbohydrate,total_sugar,free_sugar,protein,fiber,salt FROM foods`)
	if err != nil {
		t.Fatal(err)
	}
	totals := mustDailyNutrition(t, a.db, "2026-09-26")
	if totals.Carbohydrate != 140 || totals.Protein != 60 || totals.FreeSugar != nil {
		t.Fatalf("unexpected totals: %#v", totals)
	}
}

func TestDailyReplacementAndFoodSnapshot(t *testing.T) {
	a := testApp(t)
	h := a.routes()
	doForm(t, h, "POST", "/body", url.Values{"csrf": {"csrf"}, "entry_date": {"2026-09-26"}, "weight_kg": {"80"}, "waist_cm": {"90"}}, true, http.StatusSeeOther)
	doForm(t, h, "POST", "/body", url.Values{"csrf": {"csrf"}, "entry_date": {"2026-09-26"}, "weight_kg": {"81"}, "waist_cm": {"91"}}, true, http.StatusSeeOther)
	var count int
	var weight float64
	if err := a.db.QueryRow("SELECT COUNT(*),weight_kg FROM body_entries").Scan(&count, &weight); err != nil || count != 1 || weight != 81 {
		t.Fatalf("replacement: count=%d weight=%v err=%v", count, weight, err)
	}
	if _, err := a.db.Exec(`INSERT INTO foods(name,carbohydrate,total_sugar,protein,fiber,salt) VALUES('Oats',60,1,12,10,0)`); err != nil {
		t.Fatal(err)
	}
	doForm(t, h, "POST", "/nutrition/entries", url.Values{"csrf": {"csrf"}, "entry_date": {"2026-09-26"}, "food_id": {"1"}, "consumed_g": {"100"}}, true, http.StatusSeeOther)
	if _, err := a.db.Exec("UPDATE foods SET carbohydrate=30 WHERE id=1"); err != nil {
		t.Fatal(err)
	}
	var snapshot float64
	if err := a.db.QueryRow("SELECT carbohydrate FROM food_entries").Scan(&snapshot); err != nil || snapshot != 60 {
		t.Fatalf("snapshot = %v, %v", snapshot, err)
	}
}

func TestHTTPAuthenticationCSRFValidationEditAndDelete(t *testing.T) {
	a := testApp(t)
	h := a.routes()

	req := httptest.NewRequest("GET", "/", nil)
	res := httptest.NewRecorder()
	h.ServeHTTP(res, req)
	if res.Code != http.StatusSeeOther || res.Header().Get("Location") != "/login" {
		t.Fatalf("auth redirect: %d %q", res.Code, res.Header().Get("Location"))
	}

	doForm(t, h, "POST", "/login", url.Values{"email": {"me@example.com"}, "password": {"secret"}}, false, http.StatusSeeOther)
	doForm(t, h, "POST", "/nutrition/foods", url.Values{"name": {"Bad"}}, true, http.StatusForbidden)
	doForm(t, h, "POST", "/nutrition/foods", url.Values{"csrf": {"csrf"}, "name": {"Bad"}, "carbohydrate": {"10"}, "total_sugar": {"11"}, "protein": {"1"}, "fiber": {"1"}, "salt": {"1"}}, true, http.StatusBadRequest)
	doForm(t, h, "POST", "/nutrition/foods", url.Values{"csrf": {"csrf"}, "name": {"Oats"}, "carbohydrate": {"60"}, "total_sugar": {"1"}, "protein": {"12"}, "fiber": {"10"}, "salt": {"0"}}, true, http.StatusSeeOther)
	doForm(t, h, "POST", "/nutrition/foods/1", url.Values{"csrf": {"csrf"}, "name": {"Rolled oats"}, "carbohydrate": {"60"}, "total_sugar": {"1"}, "protein": {"12"}, "fiber": {"10"}, "salt": {"0"}}, true, http.StatusSeeOther)

	get := httptest.NewRequest("GET", "/nutrition/foods/1/delete", nil)
	get.AddCookie(&http.Cookie{Name: "session", Value: "test"})
	getRes := httptest.NewRecorder()
	h.ServeHTTP(getRes, get)
	if getRes.Code != http.StatusOK || !strings.Contains(getRes.Body.String(), "Delete food") {
		t.Fatalf("delete confirmation: %d", getRes.Code)
	}
	doForm(t, h, "POST", "/nutrition/foods/1/delete", url.Values{"csrf": {"csrf"}}, true, http.StatusSeeOther)
	var count int
	if err := a.db.QueryRow("SELECT COUNT(*) FROM foods").Scan(&count); err != nil || count != 0 {
		t.Fatalf("food was not deleted: %v", err)
	}
}

func TestPopulatedPagesRender(t *testing.T) {
	a := testApp(t)
	statements := []string{
		`UPDATE profile SET name='Alex',height_cm=180 WHERE id=1`,
		`INSERT INTO foods(name,carbohydrate,total_sugar,free_sugar,protein,fiber,salt) VALUES('Oats',60,1,0.5,12,10,0)`,
		`INSERT INTO food_entries(food_id,entry_date,consumed_g,food_name,carbohydrate,total_sugar,free_sugar,protein,fiber,salt) VALUES(1,'2026-09-26',100,'Oats',60,1,0.5,12,10,0)`,
		`INSERT INTO body_entries(entry_date,weight_kg,waist_cm) VALUES('2026-09-26',75,85)`,
		`INSERT INTO sleep_entries(entry_date,bed_time,wake_time,duration_hours) VALUES('2026-09-26','23:00','07:00',8)`,
	}
	for _, statement := range statements {
		if _, err := a.db.Exec(statement); err != nil {
			t.Fatal(err)
		}
	}
	for _, path := range []string{"/", "/nutrition", "/body", "/sleep", "/settings"} {
		req := httptest.NewRequest("GET", path, nil)
		req.AddCookie(&http.Cookie{Name: "session", Value: "test"})
		res := httptest.NewRecorder()
		a.routes().ServeHTTP(res, req)
		if res.Code != http.StatusOK || !strings.Contains(res.Body.String(), "</html>") {
			t.Errorf("%s did not render a complete page", path)
		}
	}
}

func TestDashboardCurrentAndLatestData(t *testing.T) {
	a := testApp(t)
	h := a.routes()
	getOverview := func() string {
		t.Helper()
		req := httptest.NewRequest("GET", "/", nil)
		req.AddCookie(&http.Cookie{Name: "session", Value: "test"})
		res := httptest.NewRecorder()
		h.ServeHTTP(res, req)
		if res.Code != http.StatusOK {
			t.Fatalf("overview: %d", res.Code)
		}
		return res.Body.String()
	}
	empty := getOverview()
	for _, want := range []string{"No food logged", "No measurements", "No sleep recorded", "Your daily totals start here"} {
		if !strings.Contains(empty, want) {
			t.Errorf("empty overview missing %q", want)
		}
	}
	today := time.Now().UTC().Format("2006-01-02")
	yesterday := time.Now().UTC().AddDate(0, 0, -1).Format("2006-01-02")
	doForm(t, h, "POST", "/nutrition/foods", url.Values{"csrf": {"csrf"}, "name": {"Oats"}, "carbohydrate": {"60"}, "total_sugar": {"1"}, "protein": {"12"}, "fiber": {"10"}, "salt": {"0"}}, true, http.StatusSeeOther)
	doForm(t, h, "POST", "/nutrition/entries", url.Values{"csrf": {"csrf"}, "entry_date": {today}, "food_id": {"1"}, "consumed_g": {"50"}}, true, http.StatusSeeOther)
	doForm(t, h, "POST", "/nutrition/entries", url.Values{"csrf": {"csrf"}, "entry_date": {yesterday}, "food_id": {"1"}, "consumed_g": {"100"}}, true, http.StatusSeeOther)
	doForm(t, h, "POST", "/body", url.Values{"csrf": {"csrf"}, "entry_date": {yesterday}, "weight_kg": {"75"}, "waist_cm": {"85"}}, true, http.StatusSeeOther)
	doForm(t, h, "POST", "/sleep", url.Values{"csrf": {"csrf"}, "entry_date": {today}, "bed_time": {"23:30"}, "wake_time": {"07:00"}}, true, http.StatusSeeOther)
	populated := getOverview()
	for _, want := range []string{"30.0 <span>g carbs</span>", "6.0 g protein", "75.0 <span>kg</span>", "7.5 <span>hours</span>", "Latest: <time>" + yesterday, "Oats", "Unknown", "/nutrition#entry-1"} {
		if !strings.Contains(populated, want) {
			t.Errorf("populated overview missing %q", want)
		}
	}
	if strings.Count(populated, ">Logged today</span>") != 2 || strings.Count(populated, ">Not logged today</span>") != 1 {
		t.Error("overview must distinguish today's entries from older measurements")
	}
}

func TestPWAAssets(t *testing.T) {
	h := testApp(t).routes()
	page := httptest.NewRecorder()
	h.ServeHTTP(page, httptest.NewRequest("GET", "/login", nil))
	if !strings.Contains(page.Body.String(), `rel="manifest" href="/assets/app.webmanifest" crossorigin="use-credentials"`) {
		t.Fatal("manifest link must include Pangolin credentials")
	}
	res := httptest.NewRecorder()
	h.ServeHTTP(res, httptest.NewRequest("GET", "/assets/app.webmanifest", nil))
	if res.Code != http.StatusOK {
		t.Fatalf("manifest status = %d", res.Code)
	}
	if contentType := res.Header().Get("Content-Type"); !strings.HasPrefix(contentType, "application/manifest+json") {
		t.Fatalf("manifest content type = %q", contentType)
	}
	var manifest struct {
		Name     string
		StartURL string `json:"start_url"`
		Display  string
		Icons    []struct{ Src, Sizes string }
	}
	if err := json.NewDecoder(res.Body).Decode(&manifest); err != nil || manifest.Name != "Macro Tracker" || manifest.StartURL != "/" || manifest.Display != "standalone" || len(manifest.Icons) != 2 {
		t.Fatalf("invalid manifest: %#v, %v", manifest, err)
	}
	for _, icon := range manifest.Icons {
		res = httptest.NewRecorder()
		h.ServeHTTP(res, httptest.NewRequest("GET", icon.Src, nil))
		cfg, err := png.DecodeConfig(res.Body)
		if err != nil || icon.Sizes != fmt.Sprintf("%dx%d", cfg.Width, cfg.Height) {
			t.Fatalf("invalid icon %s: %dx%d, %v", icon.Src, cfg.Width, cfg.Height, err)
		}
	}
}

func doForm(t *testing.T, h http.Handler, method, path string, values url.Values, authenticated bool, want int) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(method, path, strings.NewReader(values.Encode()))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	if authenticated {
		req.AddCookie(&http.Cookie{Name: "session", Value: "test"})
	}
	res := httptest.NewRecorder()
	h.ServeHTTP(res, req)
	if res.Code != want {
		body, _ := io.ReadAll(res.Result().Body)
		t.Fatalf("%s %s: got %d, want %d: %s", method, path, res.Code, want, body)
	}
	return res
}

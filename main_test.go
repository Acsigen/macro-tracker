package main

import (
	"database/sql"
	"encoding/json"
	"fmt"
	"html"
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
	dbPath := filepath.Join(t.TempDir(), "test.db")
	db, err := sql.Open("sqlite", dbPath)
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
		"f":     func(v float64) string { return fmt.Sprintf("%.2f", v) },
		"ptr":   optionalNumber,
		"omega": omegaAmount,
		"ratio": omegaRatio,
	}).ParseFS(files, "templates/*.html")
	if err != nil {
		t.Fatal(err)
	}
	a := &app{db: db, tpl: tpl, cfg: config{email: "me@example.com", password: "secret", dbPath: dbPath}, version: "0.1.0", location: time.UTC}
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
	for _, table := range []string{"foods", "food_entries"} {
		var oldCount, percentCount int
		if err := a.db.QueryRow("SELECT COUNT(*) FROM pragma_table_info(?) WHERE name='free_sugar'", table).Scan(&oldCount); err != nil || oldCount != 0 {
			t.Fatalf("%s free_sugar column count = %d, %v", table, oldCount, err)
		}
		if err := a.db.QueryRow("SELECT COUNT(*) FROM pragma_table_info(?) WHERE name='free_sugar_percent'", table).Scan(&percentCount); err != nil || percentCount != 1 {
			t.Fatalf("%s free_sugar_percent column count = %d, %v", table, percentCount, err)
		}
	}
}

func TestFatBandUsesSavedEnergyTarget(t *testing.T) {
	a := testApp(t)
	h := a.routes()
	doForm(t, h, "POST", "/settings", url.Values{"csrf": {"csrf"}, "energy_target": {"1800"}}, true, http.StatusSeeOther)
	food := foodForm()
	food.Set("free_sugar_percent", "40")
	seedFood(t, a, food, 0)
	entry := entryForm()
	entry.Set("entry_date", time.Now().UTC().Format("2006-01-02"))
	logReviewedFood(t, a, "/nutrition/entries", entry)
	nutrition := request(h, "GET", "/nutrition", "", "test")
	if nutrition.Code != http.StatusOK || !strings.Contains(nutrition.Body.String(), "30.00–60.00 g band") || !strings.Contains(nutrition.Body.String(), "25 g limit") {
		t.Fatalf("fat band did not use saved kcal target: %d %s", nutrition.Code, nutrition.Body.String())
	}
	w := request(h, "GET", "/dashboard", "", "test")
	if w.Code != http.StatusOK || !strings.Contains(w.Body.String(), `/assets/app.js?v=`+a.version) {
		t.Fatalf("dashboard did not render: %d %s", w.Code, w.Body.String())
	}
	_, chartAttribute, ok := strings.Cut(w.Body.String(), `data-chart="`)
	if !ok {
		t.Fatal("dashboard chart data missing")
	}
	chartAttribute, _, _ = strings.Cut(chartAttribute, `"`)
	var chart struct {
		TotalSugar []float64 `json:"totalSugar"`
		FreeSugar  []float64 `json:"freeSugar"`
		Fat        []float64 `json:"fat"`
		Donut      struct {
			Carbohydrate, TotalSugar, Protein, Fat, Fiber float64
		} `json:"donut"`
	}
	if err := json.Unmarshal([]byte(html.UnescapeString(chartAttribute)), &chart); err != nil || len(chart.FreeSugar) != 30 || len(chart.Fat) != 30 || chart.TotalSugar[29] != 10 || chart.FreeSugar[29] != 4 || chart.Fat[29] != 7 || chart.Donut.Carbohydrate != 60 || chart.Donut.TotalSugar != 10 || chart.Donut.Protein != 12 || chart.Donut.Fat != 7 || chart.Donut.Fiber != 10 {
		t.Fatalf("nutrition chart values: %+v, %v", chart, err)
	}
	js := request(h, "GET", "/assets/app.js?v="+a.version, "", "")
	if js.Code != http.StatusOK || !strings.Contains(js.Body.String(), `"free-sugar": ["Free sugar"`) || !strings.Contains(js.Body.String(), `"total-sugar": ["Total sugar"`) {
		t.Fatalf("versioned chart script: %d", js.Code)
	}
}

func TestNutritionSearchPaginationAndSuggestions(t *testing.T) {
	a := testApp(t)
	for i := 1; i <= 25; i++ {
		name := fmt.Sprintf("Pantry food %02d", i)
		result, err := a.db.Exec(`INSERT INTO foods(name,carbohydrate,total_sugar,free_sugar_percent,protein,fat,fiber,salt) VALUES(?,60,10,0,12,7,10,0)`, name)
		if err != nil {
			t.Fatal(err)
		}
		id, _ := result.LastInsertId()
		if _, err := a.db.Exec(`INSERT INTO food_entries(food_id,entry_date,consumed_g,food_name,carbohydrate,total_sugar,free_sugar_percent,protein,fat,fiber,salt) VALUES(?,'2026-09-29',100,?,60,10,0,12,7,10,0)`, id, name); err != nil {
			t.Fatal(err)
		}
	}
	h := a.routes()
	first := request(h, "GET", "/nutrition?food_q=Pantry&entry_q=Pantry", "", "test")
	if first.Code != http.StatusOK || strings.Count(first.Body.String(), `action="/nutrition/foods/`) != 20 || strings.Count(first.Body.String(), `id="entry-`) != 20 || !strings.Contains(first.Body.String(), "food_page=2") || !strings.Contains(first.Body.String(), "entry_page=2") {
		t.Fatalf("first page was not limited: %d", first.Code)
	}
	second := request(h, "GET", "/nutrition?food_q=Pantry&food_page=2&entry_q=Pantry&entry_page=2", "", "test")
	if second.Code != http.StatusOK || strings.Count(second.Body.String(), `action="/nutrition/foods/`) != 5 || strings.Count(second.Body.String(), `id="entry-`) != 5 || !strings.Contains(second.Body.String(), "Page 2") {
		t.Fatalf("second page was not rendered: %d", second.Code)
	}
	filtered := request(h, "GET", "/nutrition?food_q=food+25&entry_q=food+25", "", "test")
	if filtered.Code != http.StatusOK || !strings.Contains(filtered.Body.String(), "Pantry food 25") || strings.Contains(filtered.Body.String(), "Pantry food 24") {
		t.Fatalf("food search did not filter the lists: %d", filtered.Code)
	}
	suggestions := request(h, "GET", "/nutrition/foods/search?food_name=Pantry", "", "test")
	if suggestions.Code != http.StatusOK || strings.Count(suggestions.Body.String(), "<option") != 10 || strings.Contains(suggestions.Body.String(), "Pantry food 11") {
		t.Fatalf("suggestions were not limited: %d %s", suggestions.Code, suggestions.Body.String())
	}
	blankSuggestions := request(h, "GET", "/nutrition/foods/search?food_name=", "", "test")
	if blankSuggestions.Code != http.StatusOK || strings.Contains(blankSuggestions.Body.String(), "<option") {
		t.Fatalf("blank search returned unrelated foods: %d %s", blankSuggestions.Code, blankSuggestions.Body.String())
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

func TestNutritionScalingAndUnknownFat(t *testing.T) {
	a := testApp(t)
	_, err := a.db.Exec(`INSERT INTO foods(name,carbohydrate,total_sugar,free_sugar_percent,protein,fat,fiber,salt) VALUES('Milk',50,10,0,20,5,8,1),('Soda',20,5,100,10,NULL,2,0.5)`)
	if err != nil {
		t.Fatal(err)
	}
	_, err = a.db.Exec(`INSERT INTO food_entries(food_id,entry_date,consumed_g,food_name,carbohydrate,total_sugar,free_sugar_percent,protein,fat,fiber,salt) SELECT id,'2026-09-26',200,name,carbohydrate,total_sugar,free_sugar_percent,protein,fat,fiber,salt FROM foods`)
	if err != nil {
		t.Fatal(err)
	}
	totals := mustDailyNutrition(t, a.db, "2026-09-26")
	if totals.Carbohydrate != 140 || totals.TotalSugar != 30 || totals.FreeSugar != 10 || totals.Protein != 60 || totals.Fat != nil {
		t.Fatalf("unexpected totals: %#v", totals)
	}
}

func TestFoodLibraryPreservesLinkedEntries(t *testing.T) {
	a := testApp(t)
	_, err := a.db.Exec(`INSERT INTO foods(name,carbohydrate,total_sugar,free_sugar_percent,protein,fat,fiber,salt) VALUES('Oats',60,1,0,12,7,10,0);
		INSERT INTO food_entries(food_id,entry_date,consumed_g,food_name,carbohydrate,total_sugar,protein,fiber,salt) VALUES(1,'2026-09-28',100,'Old oats',50,1,12,10,0);
		UPDATE foods SET carbohydrate=50,free_sugar_percent=50,fat=9 WHERE id=1`)
	if err != nil {
		t.Fatal(err)
	}
	if totals := mustDailyNutrition(t, a.db, "2026-09-28"); totals.Fat != nil || totals.Carbohydrate != 50 || totals.FreeSugar != 0 {
		t.Fatalf("preserved totals = %+v", totals)
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
	execSQL(t, a.db, "UPDATE foods SET fat=7,omega3=0,omega6=0,analysis_source='ai' WHERE id=1")
	logReviewedFood(t, a, "/nutrition/entries", url.Values{"csrf": {"csrf"}, "entry_date": {"2026-09-26"}, "food_name": {"Oats"}, "consumed_g": {"100"}})
	if _, err := a.db.Exec("UPDATE foods SET carbohydrate=30 WHERE id=1"); err != nil {
		t.Fatal(err)
	}
	var snapshot float64
	if err := a.db.QueryRow("SELECT carbohydrate FROM food_entries").Scan(&snapshot); err != nil || snapshot != 60 {
		t.Fatalf("preserved carbohydrate = %v, %v", snapshot, err)
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
	doForm(t, h, "POST", "/nutrition/foods", url.Values{"csrf": {"csrf"}, "name": {"Bad"}, "carbohydrate": {"10"}, "total_sugar": {"11"}, "protein": {"1"}, "fat": {"1"}, "fiber": {"1"}, "salt": {"1"}}, true, http.StatusBadRequest)
	seedFood(t, a, url.Values{"csrf": {"csrf"}, "name": {"Oats"}, "carbohydrate": {"60"}, "total_sugar": {"1"}, "protein": {"12"}, "fat": {"7"}, "fiber": {"10"}, "salt": {"0"}}, 0)
	seedFood(t, a, url.Values{"csrf": {"csrf"}, "name": {"Rolled oats"}, "carbohydrate": {"60"}, "total_sugar": {"1"}, "protein": {"12"}, "fat": {"7"}, "fiber": {"10"}, "salt": {"0"}}, 1)

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
		`INSERT INTO foods(name,carbohydrate,total_sugar,protein,fat,fiber,salt) VALUES('Oats',60,1,12,7,10,0)`,
		`INSERT INTO food_entries(food_id,entry_date,consumed_g,food_name,carbohydrate,total_sugar,protein,fat,fiber,salt) VALUES(1,'2026-09-26',100,'Oats',60,1,12,7,10,0)`,
		`INSERT INTO body_entries(entry_date,weight_kg,waist_cm) VALUES('2026-09-26',75,85)`,
		`INSERT INTO sleep_entries(entry_date,bed_time,wake_time,duration_hours) VALUES('2026-09-26','23:00','07:00',8)`,
	}
	for _, statement := range statements {
		if _, err := a.db.Exec(statement); err != nil {
			t.Fatal(err)
		}
	}
	for _, path := range []string{"/", "/dashboard", "/nutrition", "/body", "/sleep", "/settings"} {
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
	seedFood(t, a, url.Values{"csrf": {"csrf"}, "name": {"Oats"}, "carbohydrate": {"60"}, "total_sugar": {"1"}, "protein": {"12"}, "fat": {"7"}, "fiber": {"10"}, "salt": {"0"}}, 0)
	logReviewedFood(t, a, "/nutrition/entries", url.Values{"csrf": {"csrf"}, "entry_date": {today}, "food_name": {"Oats"}, "consumed_g": {"50"}})
	logReviewedFood(t, a, "/nutrition/entries", url.Values{"csrf": {"csrf"}, "entry_date": {yesterday}, "food_name": {"Oats"}, "consumed_g": {"100"}})
	doForm(t, h, "POST", "/body", url.Values{"csrf": {"csrf"}, "entry_date": {yesterday}, "weight_kg": {"75"}, "waist_cm": {"85"}}, true, http.StatusSeeOther)
	doForm(t, h, "POST", "/sleep", url.Values{"csrf": {"csrf"}, "entry_date": {today}, "bed_time": {"23:30"}, "wake_time": {"07:00"}}, true, http.StatusSeeOther)
	populated := getOverview()
	for _, want := range []string{"30.00 <span>g carbs</span>", "6.00 g protein", "3.50 g fat", "75.00 <span>kg</span>", "7.50 <span>hours</span>", "Latest: <time>" + yesterday, "Oats", "/nutrition#entry-1"} {
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

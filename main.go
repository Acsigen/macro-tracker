package main

import (
	"context"
	"crypto/rand"
	"crypto/subtle"
	"database/sql"
	"embed"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"html/template"
	"io/fs"
	"log/slog"
	"math"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"slices"
	"sort"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"
	_ "time/tzdata"
	"unicode/utf16"
	"unicode/utf8"

	"modernc.org/sqlite"
	sqlite3 "modernc.org/sqlite/lib"
)

//go:embed templates/*.html assets/* migrations/*.sql VERSION
var files embed.FS

type config struct {
	addr, dbPath, email, password, timezone string
	secureCookie                            bool
}

type session struct {
	csrf    string
	expires time.Time
}

type app struct {
	db       *sql.DB
	tpl      *template.Template
	cfg      config
	version  string
	location *time.Location
	sessions struct {
		sync.Mutex
		m map[string]session
	}
}

type profile struct {
	Name         string
	HeightCM     *float64
	EnergyTarget float64
}

type food struct {
	ID                                                               int64
	Name                                                             string
	Carbohydrate, TotalSugar, FreeSugarPercent, Protein, Fiber, Salt float64
	Fat                                                              *float64
}

type foodEntry struct {
	ID, FoodID                                                int64
	Date, FoodName                                            string
	ConsumedG, Carbohydrate, TotalSugar, Protein, Fiber, Salt float64
	Fat                                                       *float64
}

type bodyEntry struct {
	ID                     int64
	Date                   string
	WeightKG, WaistCM      float64
	BMI                    *float64
	BMIStatus, WaistStatus string
}

type sleepEntry struct {
	ID            int64
	Date          string
	BedTime       string
	WakeTime      string
	DurationHours float64
}

type dailyStatus struct {
	Nutrition, Body, Sleep bool
}

type nutritionTotals struct {
	Carbohydrate, TotalSugar, FreeSugar, Protein, Fiber, Salt float64
	Fat                                                       *float64
	CarbMin, CarbMax, ProteinMin, ProteinMax                  float64
	FatMin, FatMax                                            float64
}

type pageData struct {
	Title, Path, CSRF, Today, Error, Version, Range, Summary, ChartJSON string
	Profile                                                             profile
	Foods                                                               []food
	FoodEntries                                                         []foodEntry
	BodyEntries                                                         []bodyEntry
	SleepEntries                                                        []sleepEntry
	Daily                                                               dailyStatus
	Totals                                                              nutritionTotals
	DeleteKind, DeleteName, DeleteAction, DeleteBack                    string
}

func main() {
	cfg, err := loadConfig()
	if err != nil {
		slog.Error("configuration error", "error", err)
		os.Exit(1)
	}
	if err := run(cfg); err != nil {
		slog.Error("server stopped", "error", err)
		os.Exit(1)
	}
}

func loadConfig() (config, error) {
	c := config{
		addr:     env("APP_ADDR", ":8080"),
		dbPath:   env("APP_DB_PATH", "macro-tracker.db"),
		email:    os.Getenv("APP_EMAIL"),
		password: os.Getenv("APP_PASSWORD"),
		timezone: env("APP_TIMEZONE", "UTC"),
	}
	if c.email == "" || c.password == "" {
		return c, errors.New("APP_EMAIL and APP_PASSWORD are required")
	}
	if raw := os.Getenv("APP_SECURE_COOKIE"); raw != "" {
		v, err := strconv.ParseBool(raw)
		if err != nil {
			return c, fmt.Errorf("APP_SECURE_COOKIE: %w", err)
		}
		c.secureCookie = v
	}
	return c, nil
}

func env(name, fallback string) string {
	if value := os.Getenv(name); value != "" {
		return value
	}
	return fallback
}

func run(cfg config) error {
	if dir := filepath.Dir(cfg.dbPath); dir != "." {
		if err := os.MkdirAll(dir, 0o750); err != nil {
			return err
		}
	}
	db, err := sql.Open("sqlite", cfg.dbPath)
	if err != nil {
		return err
	}
	defer db.Close()
	db.SetMaxOpenConns(1)
	if _, err = db.Exec("PRAGMA foreign_keys=ON; PRAGMA busy_timeout=5000; PRAGMA journal_mode=WAL;"); err != nil {
		return err
	}
	if err = migrate(db); err != nil {
		return err
	}
	loc, err := time.LoadLocation(cfg.timezone)
	if err != nil {
		return fmt.Errorf("APP_TIMEZONE: %w", err)
	}
	versionBytes, _ := files.ReadFile("VERSION")
	tpl, err := template.New("pages").Funcs(template.FuncMap{
		"f":   func(v float64) string { return strconv.FormatFloat(v, 'f', 1, 64) },
		"ptr": optionalNumber,
	}).ParseFS(files, "templates/*.html")
	if err != nil {
		return err
	}
	a := &app{db: db, tpl: tpl, cfg: cfg, version: strings.TrimSpace(string(versionBytes)), location: loc}
	a.sessions.m = make(map[string]session)

	srv := &http.Server{
		Addr:              cfg.addr,
		Handler:           a.routes(),
		ReadHeaderTimeout: 5 * time.Second,
		ReadTimeout:       15 * time.Second,
		WriteTimeout:      30 * time.Second,
		IdleTimeout:       60 * time.Second,
	}
	errCh := make(chan error, 1)
	go func() {
		slog.Info("listening", "addr", cfg.addr, "version", a.version)
		errCh <- srv.ListenAndServe()
	}()
	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()
	select {
	case err = <-errCh:
		if !errors.Is(err, http.ErrServerClosed) {
			return err
		}
		return nil
	case <-ctx.Done():
	}
	shutdownCtx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	return srv.Shutdown(shutdownCtx)
}

func migrate(db *sql.DB) error {
	if _, err := db.Exec(`CREATE TABLE IF NOT EXISTS schema_migrations (version TEXT PRIMARY KEY, applied_at TEXT NOT NULL DEFAULT CURRENT_TIMESTAMP)`); err != nil {
		return err
	}
	names, err := fs.Glob(files, "migrations/*.sql")
	if err != nil {
		return err
	}
	sort.Strings(names)
	for _, name := range names {
		version := strings.TrimSuffix(filepath.Base(name), ".sql")
		var exists int
		if err := db.QueryRow("SELECT COUNT(*) FROM schema_migrations WHERE version=?", version).Scan(&exists); err != nil {
			return err
		}
		if exists != 0 {
			continue
		}
		body, err := files.ReadFile(name)
		if err != nil {
			return err
		}
		tx, err := db.Begin()
		if err != nil {
			return err
		}
		if _, err = tx.Exec(string(body)); err == nil {
			_, err = tx.Exec("INSERT INTO schema_migrations(version) VALUES (?)", version)
		}
		if err != nil {
			tx.Rollback()
			return fmt.Errorf("migration %s: %w", version, err)
		}
		if err := tx.Commit(); err != nil {
			return err
		}
	}
	return nil
}

func (a *app) routes() http.Handler {
	mux := http.NewServeMux()
	assets, _ := fs.Sub(files, "assets")
	mux.Handle("GET /assets/", http.StripPrefix("/assets/", http.FileServerFS(assets)))
	manifest, _ := files.ReadFile("assets/app.webmanifest")
	mux.HandleFunc("GET /assets/app.webmanifest", func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/manifest+json")
		w.Write(manifest)
	})
	mux.HandleFunc("GET /healthz", a.health)
	mux.HandleFunc("GET /login", a.loginPage)
	mux.HandleFunc("POST /login", a.login)
	mux.HandleFunc("POST /logout", a.auth(a.csrf(a.logout)))
	mux.HandleFunc("GET /", a.auth(a.dashboard))
	mux.HandleFunc("GET /nutrition", a.auth(a.nutritionPage))
	mux.HandleFunc("POST /nutrition/foods", a.auth(a.csrf(a.saveFood)))
	mux.HandleFunc("POST /nutrition/foods/{id}", a.auth(a.csrf(a.saveFood)))
	mux.HandleFunc("GET /nutrition/foods/{id}/delete", a.auth(a.deletePage("food")))
	mux.HandleFunc("POST /nutrition/foods/{id}/delete", a.auth(a.csrf(a.deleteRecord("foods", "/nutrition"))))
	mux.HandleFunc("POST /nutrition/entries", a.auth(a.csrf(a.saveFoodEntry)))
	mux.HandleFunc("POST /nutrition/entries/{id}", a.auth(a.csrf(a.saveFoodEntry)))
	mux.HandleFunc("GET /nutrition/entries/{id}/delete", a.auth(a.deletePage("food entry")))
	mux.HandleFunc("POST /nutrition/entries/{id}/delete", a.auth(a.csrf(a.deleteRecord("food_entries", "/nutrition"))))
	mux.HandleFunc("GET /body", a.auth(a.bodyPage))
	mux.HandleFunc("POST /body", a.auth(a.csrf(a.saveBody)))
	mux.HandleFunc("POST /body/{id}", a.auth(a.csrf(a.saveBody)))
	mux.HandleFunc("GET /body/{id}/delete", a.auth(a.deletePage("body entry")))
	mux.HandleFunc("POST /body/{id}/delete", a.auth(a.csrf(a.deleteRecord("body_entries", "/body"))))
	mux.HandleFunc("GET /sleep", a.auth(a.sleepPage))
	mux.HandleFunc("POST /sleep", a.auth(a.csrf(a.saveSleep)))
	mux.HandleFunc("POST /sleep/{id}", a.auth(a.csrf(a.saveSleep)))
	mux.HandleFunc("GET /sleep/{id}/delete", a.auth(a.deletePage("sleep entry")))
	mux.HandleFunc("POST /sleep/{id}/delete", a.auth(a.csrf(a.deleteRecord("sleep_entries", "/sleep"))))
	mux.HandleFunc("GET /settings", a.auth(a.settingsPage))
	mux.HandleFunc("POST /settings", a.auth(a.csrf(a.saveSettings)))
	return a.security(a.logging(mux))
}

func (a *app) security(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Security-Policy", "default-src 'self'; script-src 'self'; style-src 'self'; font-src 'self'; img-src 'self' data:; connect-src 'self'; base-uri 'none'; form-action 'self'; frame-ancestors 'none'")
		w.Header().Set("Referrer-Policy", "no-referrer")
		w.Header().Set("X-Content-Type-Options", "nosniff")
		w.Header().Set("X-Frame-Options", "DENY")
		w.Header().Set("Permissions-Policy", "camera=(), microphone=(), geolocation=()")
		if r.Method == http.MethodPost {
			r.Body = http.MaxBytesReader(w, r.Body, 1<<20)
		}
		next.ServeHTTP(w, r)
	})
}

type statusWriter struct {
	http.ResponseWriter
	status int
}

func (w *statusWriter) WriteHeader(code int) { w.status = code; w.ResponseWriter.WriteHeader(code) }

func (a *app) logging(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		start := time.Now()
		sw := &statusWriter{ResponseWriter: w, status: 200}
		next.ServeHTTP(sw, r)
		slog.Info("request", "method", r.Method, "path", r.URL.Path, "status", sw.status, "duration", time.Since(start))
	})
}

func (a *app) health(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "application/json")
	status := "ok"
	if err := a.db.PingContext(r.Context()); err != nil {
		status = "error"
		w.WriteHeader(http.StatusServiceUnavailable)
	}
	json.NewEncoder(w).Encode(map[string]string{"status": status, "version": a.version})
}

func (a *app) loginPage(w http.ResponseWriter, r *http.Request) {
	if _, ok := a.currentSession(r); ok {
		http.Redirect(w, r, "/", http.StatusSeeOther)
		return
	}
	a.render(w, "login", pageData{Title: "Access", Version: a.version})
}

func (a *app) login(w http.ResponseWriter, r *http.Request) {
	if err := r.ParseForm(); err != nil {
		http.Error(w, "Invalid form", http.StatusBadRequest)
		return
	}
	if subtle.ConstantTimeCompare([]byte(r.FormValue("email")), []byte(a.cfg.email)) != 1 || subtle.ConstantTimeCompare([]byte(r.FormValue("password")), []byte(a.cfg.password)) != 1 {
		w.WriteHeader(http.StatusUnauthorized)
		a.render(w, "login", pageData{Title: "Access", Error: "The email or password is incorrect.", Version: a.version})
		return
	}
	token, csrf, err := randomToken(), randomToken(), error(nil)
	if token == "" || csrf == "" {
		err = errors.New("random token failure")
	}
	if err != nil {
		http.Error(w, "Authentication failed", http.StatusInternalServerError)
		return
	}
	a.sessions.Lock()
	a.sessions.m[token] = session{csrf: csrf, expires: time.Now().Add(12 * time.Hour)}
	a.sessions.Unlock()
	http.SetCookie(w, &http.Cookie{Name: "session", Value: token, Path: "/", HttpOnly: true, Secure: a.cfg.secureCookie, SameSite: http.SameSiteStrictMode, MaxAge: 12 * 60 * 60})
	http.Redirect(w, r, "/", http.StatusSeeOther)
}

func randomToken() string {
	b := make([]byte, 32)
	if _, err := rand.Read(b); err != nil {
		return ""
	}
	return hex.EncodeToString(b)
}

func (a *app) currentSession(r *http.Request) (session, bool) {
	cookie, err := r.Cookie("session")
	if err != nil {
		return session{}, false
	}
	a.sessions.Lock()
	defer a.sessions.Unlock()
	s, ok := a.sessions.m[cookie.Value]
	if !ok || time.Now().After(s.expires) {
		delete(a.sessions.m, cookie.Value)
		return session{}, false
	}
	return s, true
}

func (a *app) auth(next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if _, ok := a.currentSession(r); !ok {
			http.Redirect(w, r, "/login", http.StatusSeeOther)
			return
		}
		next(w, r)
	}
}

func (a *app) csrf(next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if err := r.ParseForm(); err != nil {
			code := http.StatusBadRequest
			var tooLarge *http.MaxBytesError
			if errors.As(err, &tooLarge) {
				code = http.StatusRequestEntityTooLarge
			}
			http.Error(w, "Invalid form", code)
			return
		}
		s, ok := a.currentSession(r)
		token := r.PostForm.Get("csrf")
		if !ok || token == "" || subtle.ConstantTimeCompare([]byte(token), []byte(s.csrf)) != 1 {
			http.Error(w, "Invalid CSRF token", http.StatusForbidden)
			return
		}
		next(w, r)
	}
}

func (a *app) logout(w http.ResponseWriter, r *http.Request) {
	if cookie, err := r.Cookie("session"); err == nil {
		a.sessions.Lock()
		delete(a.sessions.m, cookie.Value)
		a.sessions.Unlock()
	}
	http.SetCookie(w, &http.Cookie{Name: "session", Path: "/", MaxAge: -1, HttpOnly: true, Secure: a.cfg.secureCookie, SameSite: http.SameSiteStrictMode})
	http.Redirect(w, r, "/login", http.StatusSeeOther)
}

func (a *app) baseData(r *http.Request, title string) pageData {
	s, _ := a.currentSession(r)
	return pageData{Title: title, Path: r.URL.Path, CSRF: s.csrf, Today: time.Now().In(a.location).Format("2006-01-02"), Version: a.version}
}

func (a *app) render(w http.ResponseWriter, name string, data pageData) {
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	if err := a.tpl.ExecuteTemplate(w, name, data); err != nil {
		slog.Error("render", "template", name, "error", err)
	}
}

func (a *app) getProfile(ctx context.Context) (profile, error) {
	var p profile
	var height sql.NullFloat64
	err := a.db.QueryRowContext(ctx, "SELECT name,height_cm,energy_target FROM profile WHERE id=1").Scan(&p.Name, &height, &p.EnergyTarget)
	if height.Valid {
		p.HeightCM = &height.Float64
	}
	return p, err
}

func (a *app) dashboard(w http.ResponseWriter, r *http.Request) {
	d := a.baseData(r, "Overview")
	var err error
	if d.Profile, err = a.getProfile(r.Context()); databaseError(w, err) {
		return
	}
	if d.FoodEntries, err = a.listFoodEntries(r.Context(), 5); err != nil {
		http.Error(w, "Food entries could not be loaded", http.StatusInternalServerError)
		return
	}
	if d.BodyEntries, err = a.listBody(r.Context(), 1, d.Profile); err != nil {
		http.Error(w, "Body measurements could not be loaded", http.StatusInternalServerError)
		return
	}
	if d.SleepEntries, err = a.listSleep(r.Context(), 1); err != nil {
		http.Error(w, "Sleep records could not be loaded", http.StatusInternalServerError)
		return
	}
	if d.Totals, err = dailyNutrition(r.Context(), a.db, d.Today); databaseError(w, err) {
		return
	}
	err = a.db.QueryRowContext(r.Context(), `SELECT
 EXISTS(SELECT 1 FROM food_entries WHERE entry_date=?),
 EXISTS(SELECT 1 FROM body_entries WHERE entry_date=?),
 EXISTS(SELECT 1 FROM sleep_entries WHERE entry_date=?)`, d.Today, d.Today, d.Today).Scan(&d.Daily.Nutrition, &d.Daily.Body, &d.Daily.Sleep)
	if databaseError(w, err) {
		return
	}
	a.render(w, "dashboard", d)
}

func parseRange(r *http.Request) int {
	days, _ := strconv.Atoi(r.URL.Query().Get("days"))
	if slices.Contains([]int{7, 30, 90, 365}, days) {
		return days
	}
	return 30
}

func (a *app) nutritionPage(w http.ResponseWriter, r *http.Request) {
	d := a.baseData(r, "Nutrition")
	var err error
	if d.Profile, err = a.getProfile(r.Context()); databaseError(w, err) {
		return
	}
	if d.Foods, err = a.listFoods(r.Context()); databaseError(w, err) {
		return
	}
	if d.FoodEntries, err = a.listFoodEntries(r.Context(), 100); databaseError(w, err) {
		return
	}
	days := parseRange(r)
	d.Range = strconv.Itoa(days)
	labels, carbs, freeSugar, protein, fat, fiber, salt, err := a.nutritionSeries(r.Context(), days)
	if databaseError(w, err) {
		return
	}
	t := d.Profile.EnergyTarget
	d.ChartJSON = chartJSON(map[string]any{"labels": labels, "carbohydrate": carbs, "freeSugar": freeSugar, "protein": protein, "fat": fat, "fiber": fiber, "salt": salt, "carbMin": t * .45 / 4, "carbMax": t * .75 / 4, "proteinMin": t * .10 / 4, "proteinMax": t * .15 / 4, "fatMin": t * .15 / 9, "fatMax": t * .30 / 9})
	if d.Totals, err = dailyNutrition(r.Context(), a.db, d.Today); databaseError(w, err) {
		return
	}
	d.Totals.CarbMin, d.Totals.CarbMax = t*.45/4, t*.75/4
	d.Totals.ProteinMin, d.Totals.ProteinMax = t*.10/4, t*.15/4
	d.Totals.FatMin, d.Totals.FatMax = t*.15/9, t*.30/9
	d.Summary = fmt.Sprintf("The chart shows %d days. It compares daily totals with the selected nutrition references.", days)
	a.render(w, "nutrition", d)
}

func (a *app) listFoods(ctx context.Context) ([]food, error) {
	rows, err := a.db.QueryContext(ctx, "SELECT id,name,carbohydrate,total_sugar,free_sugar_percent,protein,fat,fiber,salt FROM foods ORDER BY name")
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var result []food
	for rows.Next() {
		var f food
		var fat sql.NullFloat64
		if err := rows.Scan(&f.ID, &f.Name, &f.Carbohydrate, &f.TotalSugar, &f.FreeSugarPercent, &f.Protein, &fat, &f.Fiber, &f.Salt); err != nil {
			return nil, err
		}
		if fat.Valid {
			f.Fat = &fat.Float64
		}
		result = append(result, f)
	}
	return result, rows.Err()
}

func (a *app) listFoodEntries(ctx context.Context, limit int) ([]foodEntry, error) {
	rows, err := a.db.QueryContext(ctx, "SELECT id,COALESCE(food_id,0),entry_date,consumed_g,food_name,carbohydrate,total_sugar,protein,fat,fiber,salt FROM food_entries ORDER BY entry_date DESC,id DESC LIMIT ?", limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var result []foodEntry
	for rows.Next() {
		var e foodEntry
		var fat sql.NullFloat64
		if err := rows.Scan(&e.ID, &e.FoodID, &e.Date, &e.ConsumedG, &e.FoodName, &e.Carbohydrate, &e.TotalSugar, &e.Protein, &fat, &e.Fiber, &e.Salt); err != nil {
			return nil, err
		}
		if fat.Valid {
			e.Fat = &fat.Float64
		}
		result = append(result, e)
	}
	return result, rows.Err()
}

func formFloat(r *http.Request, name string, min, max float64) (float64, error) {
	v, err := strconv.ParseFloat(r.FormValue(name), 64)
	if err != nil || math.IsNaN(v) || math.IsInf(v, 0) || v < min || v > max {
		return 0, fmt.Errorf("%s is invalid", name)
	}
	return v, nil
}

func optionalFloat(r *http.Request, name string, min, max float64) (*float64, error) {
	if strings.TrimSpace(r.FormValue(name)) == "" {
		return nil, nil
	}
	v, err := formFloat(r, name, min, max)
	return &v, err
}

func parseID(r *http.Request) (int64, error) {
	if r.PathValue("id") == "" {
		return 0, nil
	}
	id, err := strconv.ParseInt(r.PathValue("id"), 10, 64)
	if err != nil || id <= 0 {
		return 0, errors.New("invalid record")
	}
	return id, nil
}

func validDate(value string) bool {
	date, err := time.Parse("2006-01-02", value)
	return err == nil && date.Year() > 0 && date.Format("2006-01-02") == value
}

func validName(value string) bool {
	return utf8.ValidString(value) && len(utf16.Encode([]rune(value))) <= 120
}

func optionalNumber(value *float64) string {
	if value == nil {
		return ""
	}
	return strconv.FormatFloat(*value, 'f', -1, 64)
}

func databaseError(w http.ResponseWriter, err error) bool {
	if err == nil {
		return false
	}
	slog.Error("database", "error", err)
	http.Error(w, "Data could not be loaded. Please try again.", http.StatusInternalServerError)
	return true
}

func savedRecord(w http.ResponseWriter, r *http.Request, result sql.Result, err error, redirect string) {
	if err != nil {
		var constraint *sqlite.Error
		if errors.As(err, &constraint) && constraint.Code() == sqlite3.SQLITE_CONSTRAINT_UNIQUE {
			http.Error(w, "A record with that date or name already exists. Your changes were not saved.", http.StatusConflict)
		} else {
			http.Error(w, "Record could not be saved", http.StatusInternalServerError)
		}
		return
	}
	n, err := result.RowsAffected()
	if databaseError(w, err) {
		return
	}
	if n == 0 {
		http.NotFound(w, r)
		return
	}
	http.Redirect(w, r, redirect, http.StatusSeeOther)
}

func (a *app) saveFood(w http.ResponseWriter, r *http.Request) {
	if err := r.ParseForm(); err != nil {
		http.Error(w, "Invalid form", 400)
		return
	}
	id, err := parseID(r)
	if err != nil {
		http.Error(w, err.Error(), 400)
		return
	}
	name := strings.TrimSpace(r.FormValue("name"))
	if name == "" || !validName(name) {
		http.Error(w, "name is invalid", 400)
		return
	}
	carb, e1 := formFloat(r, "carbohydrate", 0, 100)
	sugar, e2 := formFloat(r, "total_sugar", 0, 100)
	freeSugarPercent := 0.0
	var e7 error
	if r.FormValue("free_sugar_percent") != "" {
		freeSugarPercent, e7 = formFloat(r, "free_sugar_percent", 0, 100)
	}
	protein, e3 := formFloat(r, "protein", 0, 100)
	fat, e4 := formFloat(r, "fat", 0, 100)
	fiber, e5 := formFloat(r, "fiber", 0, 100)
	salt, e6 := formFloat(r, "salt", 0, 100)
	if err = errors.Join(e1, e2, e3, e4, e5, e6, e7); err != nil || sugar > carb {
		http.Error(w, "Nutrient values are invalid", 400)
		return
	}
	var result sql.Result
	if id == 0 {
		result, err = a.db.ExecContext(r.Context(), "INSERT INTO foods(name,carbohydrate,total_sugar,free_sugar_percent,protein,fat,fiber,salt) VALUES(?,?,?,?,?,?,?,?)", name, carb, sugar, freeSugarPercent, protein, fat, fiber, salt)
	} else {
		result, err = a.db.ExecContext(r.Context(), "UPDATE foods SET name=?,carbohydrate=?,total_sugar=?,free_sugar_percent=?,protein=?,fat=?,fiber=?,salt=? WHERE id=?", name, carb, sugar, freeSugarPercent, protein, fat, fiber, salt, id)
	}
	savedRecord(w, r, result, err, "/nutrition")
}

func (a *app) saveFoodEntry(w http.ResponseWriter, r *http.Request) {
	if err := r.ParseForm(); err != nil {
		http.Error(w, "Invalid form", 400)
		return
	}
	id, err := parseID(r)
	if err != nil {
		http.Error(w, err.Error(), 400)
		return
	}
	var foodID int64
	if raw := r.FormValue("food_id"); raw != "" {
		foodID, err = strconv.ParseInt(raw, 10, 64)
		if err != nil || foodID <= 0 {
			http.Error(w, "food is invalid", 400)
			return
		}
	}
	if id == 0 && foodID == 0 {
		http.Error(w, "food is invalid", 400)
		return
	}
	date := r.FormValue("entry_date")
	if !validDate(date) {
		http.Error(w, "date is invalid", 400)
		return
	}
	grams, err := formFloat(r, "consumed_g", 0.01, 100000)
	if err != nil {
		http.Error(w, err.Error(), 400)
		return
	}
	ctx := r.Context()
	tx, err := a.db.BeginTx(ctx, nil)
	if databaseError(w, err) {
		return
	}
	defer tx.Rollback()
	var f food
	var fat sql.NullFloat64
	if id > 0 {
		err = tx.QueryRowContext(ctx, "SELECT COALESCE(food_id,0),food_name,carbohydrate,total_sugar,free_sugar_percent,protein,fat,fiber,salt FROM food_entries WHERE id=?", id).Scan(&f.ID, &f.Name, &f.Carbohydrate, &f.TotalSugar, &f.FreeSugarPercent, &f.Protein, &fat, &f.Fiber, &f.Salt)
		if errors.Is(err, sql.ErrNoRows) {
			http.NotFound(w, r)
			return
		}
		if databaseError(w, err) {
			return
		}
	}
	if id == 0 || foodID != f.ID {
		err = tx.QueryRowContext(ctx, "SELECT id,name,carbohydrate,total_sugar,free_sugar_percent,protein,fat,fiber,salt FROM foods WHERE id=?", foodID).Scan(&f.ID, &f.Name, &f.Carbohydrate, &f.TotalSugar, &f.FreeSugarPercent, &f.Protein, &fat, &f.Fiber, &f.Salt)
		if errors.Is(err, sql.ErrNoRows) {
			http.Error(w, "food is invalid", 400)
			return
		}
		if databaseError(w, err) {
			return
		}
	}
	var sourceID any
	if f.ID != 0 {
		sourceID = f.ID
	}
	var result sql.Result
	if id == 0 {
		result, err = tx.ExecContext(ctx, "INSERT INTO food_entries(food_id,entry_date,consumed_g,food_name,carbohydrate,total_sugar,free_sugar_percent,protein,fat,fiber,salt) VALUES(?,?,?,?,?,?,?,?,?,?,?)", sourceID, date, grams, f.Name, f.Carbohydrate, f.TotalSugar, f.FreeSugarPercent, f.Protein, nullable(fat), f.Fiber, f.Salt)
	} else {
		result, err = tx.ExecContext(ctx, "UPDATE food_entries SET food_id=?,entry_date=?,consumed_g=?,food_name=?,carbohydrate=?,total_sugar=?,free_sugar_percent=?,protein=?,fat=?,fiber=?,salt=? WHERE id=?", sourceID, date, grams, f.Name, f.Carbohydrate, f.TotalSugar, f.FreeSugarPercent, f.Protein, nullable(fat), f.Fiber, f.Salt, id)
	}
	if err == nil {
		err = tx.Commit()
	}
	savedRecord(w, r, result, err, "/nutrition")
}

func nullable(v sql.NullFloat64) any {
	if v.Valid {
		return v.Float64
	}
	return nil
}

func dailyNutrition(ctx context.Context, db *sql.DB, date string) (nutritionTotals, error) {
	var carbs, sugar, freeSugar, protein, fiber, salt float64
	var fat sql.NullFloat64
	var unknownFat int
	err := db.QueryRowContext(ctx, `SELECT COALESCE(SUM(carbohydrate*consumed_g/100),0),COALESCE(SUM(total_sugar*consumed_g/100),0),COALESCE(SUM(total_sugar*free_sugar_percent*consumed_g/10000),0),COALESCE(SUM(protein*consumed_g/100),0),SUM(fat*consumed_g/100),COALESCE(SUM(fiber*consumed_g/100),0),COALESCE(SUM(salt*consumed_g/100),0),COALESCE(SUM(fat IS NULL),0) FROM food_entries WHERE entry_date=?`, date).Scan(&carbs, &sugar, &freeSugar, &protein, &fat, &fiber, &salt, &unknownFat)
	if err != nil {
		return nutritionTotals{}, err
	}
	result := nutritionTotals{Carbohydrate: carbs, TotalSugar: sugar, FreeSugar: freeSugar, Protein: protein, Fiber: fiber, Salt: salt}
	if fat.Valid && unknownFat == 0 {
		result.Fat = &fat.Float64
	}
	return result, nil
}

func (a *app) nutritionSeries(ctx context.Context, days int) ([]string, []float64, []float64, []float64, []any, []float64, []float64, error) {
	labels := dateLabels(time.Now().In(a.location), days)
	carbs := make([]float64, days)
	freeSugar := make([]float64, days)
	protein := make([]float64, days)
	fat := make([]any, days)
	fiber := make([]float64, days)
	salt := make([]float64, days)
	index := map[string]int{}
	for i, v := range labels {
		index[v] = i
		fat[i] = nil
	}
	start := labels[0]
	rows, err := a.db.QueryContext(ctx, `SELECT entry_date,SUM(carbohydrate*consumed_g/100),SUM(total_sugar*free_sugar_percent*consumed_g/10000),SUM(protein*consumed_g/100),SUM(fat*consumed_g/100),SUM(fiber*consumed_g/100),SUM(salt*consumed_g/100),SUM(fat IS NULL) FROM food_entries WHERE entry_date>=? GROUP BY entry_date`, start)
	if err != nil {
		return labels, carbs, freeSugar, protein, fat, fiber, salt, err
	}
	defer rows.Close()
	for rows.Next() {
		var date string
		var c, su, p, fi, s float64
		var fa sql.NullFloat64
		var unknownFat int
		if err := rows.Scan(&date, &c, &su, &p, &fa, &fi, &s, &unknownFat); err != nil {
			return labels, carbs, freeSugar, protein, fat, fiber, salt, err
		}
		if i, ok := index[date]; ok {
			carbs[i] = c
			freeSugar[i] = su
			protein[i] = p
			fiber[i] = fi
			salt[i] = s
			if fa.Valid && unknownFat == 0 {
				fat[i] = fa.Float64
			}
		}
	}
	return labels, carbs, freeSugar, protein, fat, fiber, salt, rows.Err()
}

func dateLabels(now time.Time, days int) []string {
	result := make([]string, days)
	for i := range days {
		result[i] = now.AddDate(0, 0, i-days+1).Format("2006-01-02")
	}
	return result
}
func chartJSON(v any) string { b, _ := json.Marshal(v); return string(b) }

func (a *app) bodyPage(w http.ResponseWriter, r *http.Request) {
	d := a.baseData(r, "Body")
	var err error
	if d.Profile, err = a.getProfile(r.Context()); databaseError(w, err) {
		return
	}
	days := parseRange(r)
	d.Range = strconv.Itoa(days)
	if d.BodyEntries, err = a.listBody(r.Context(), 100, d.Profile); databaseError(w, err) {
		return
	}
	labels, weight, bmis, waists, err := a.bodySeries(r.Context(), days, d.Profile)
	if databaseError(w, err) {
		return
	}
	var waistTarget *float64
	if d.Profile.HeightCM != nil {
		v := *d.Profile.HeightCM / 2
		waistTarget = &v
	}
	d.ChartJSON = chartJSON(map[string]any{"labels": labels, "weight": weight, "bmi": bmis, "waist": waists, "waistTarget": waistTarget})
	d.Summary = fmt.Sprintf("The chart shows %d days of weight, BMI, and waist measurements.", days)
	a.render(w, "body", d)
}

func (a *app) listBody(ctx context.Context, limit int, p profile) ([]bodyEntry, error) {
	rows, err := a.db.QueryContext(ctx, "SELECT id,entry_date,weight_kg,waist_cm FROM body_entries ORDER BY entry_date DESC LIMIT ?", limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []bodyEntry
	for rows.Next() {
		var e bodyEntry
		if err := rows.Scan(&e.ID, &e.Date, &e.WeightKG, &e.WaistCM); err != nil {
			return nil, err
		}
		if p.HeightCM != nil {
			v := bmi(e.WeightKG, *p.HeightCM)
			e.BMI = &v
			e.BMIStatus = bmiBand(v)
			e.WaistStatus = waistBand(e.WaistCM, *p.HeightCM)
		}
		out = append(out, e)
	}
	return out, rows.Err()
}
func bmi(weight, heightCM float64) float64 { h := heightCM / 100; return weight / (h * h) }
func bmiBand(v float64) string {
	if v < 18.5 {
		return "Below reference"
	}
	if v < 25 {
		return "Reference range"
	}
	if v < 30 {
		return "Above reference"
	}
	return "High reference"
}
func waistBand(waist, height float64) string {
	target := height / 2
	if waist < target-4 {
		return "Below optimal range"
	}
	if waist <= target+4 {
		return "Optimal range"
	}
	return "Above optimal range"
}

func (a *app) saveBody(w http.ResponseWriter, r *http.Request) {
	if err := r.ParseForm(); err != nil {
		http.Error(w, "Invalid form", 400)
		return
	}
	id, err := parseID(r)
	if err != nil {
		http.Error(w, err.Error(), 400)
		return
	}
	date := r.FormValue("entry_date")
	if !validDate(date) {
		http.Error(w, "date is invalid", 400)
		return
	}
	weight, e1 := formFloat(r, "weight_kg", 1, 1000)
	waist, e2 := formFloat(r, "waist_cm", 1, 500)
	if errors.Join(e1, e2) != nil {
		http.Error(w, "Measurements are invalid", 400)
		return
	}
	var result sql.Result
	if id > 0 {
		result, err = a.db.ExecContext(r.Context(), "UPDATE body_entries SET entry_date=?,weight_kg=?,waist_cm=? WHERE id=?", date, weight, waist, id)
	} else {
		result, err = a.db.ExecContext(r.Context(), `INSERT INTO body_entries(entry_date,weight_kg,waist_cm) VALUES(?,?,?) ON CONFLICT(entry_date) DO UPDATE SET weight_kg=excluded.weight_kg,waist_cm=excluded.waist_cm`, date, weight, waist)
	}
	savedRecord(w, r, result, err, "/body")
}

func (a *app) bodySeries(ctx context.Context, days int, p profile) ([]string, []any, []any, []any, error) {
	labels := dateLabels(time.Now().In(a.location), days)
	weight := make([]any, days)
	bmis := make([]any, days)
	waists := make([]any, days)
	idx := map[string]int{}
	for i, v := range labels {
		idx[v] = i
	}
	rows, err := a.db.QueryContext(ctx, "SELECT entry_date,weight_kg,waist_cm FROM body_entries WHERE entry_date>=?", labels[0])
	if err != nil {
		return labels, weight, bmis, waists, err
	}
	defer rows.Close()
	for rows.Next() {
		var date string
		var w, waist float64
		if err := rows.Scan(&date, &w, &waist); err != nil {
			return labels, weight, bmis, waists, err
		}
		if i, ok := idx[date]; ok {
			weight[i] = w
			waists[i] = waist
			if p.HeightCM != nil {
				bmis[i] = bmi(w, *p.HeightCM)
			}
		}
	}
	return labels, weight, bmis, waists, rows.Err()
}

func (a *app) sleepPage(w http.ResponseWriter, r *http.Request) {
	d := a.baseData(r, "Sleep")
	var err error
	days := parseRange(r)
	d.Range = strconv.Itoa(days)
	if d.SleepEntries, err = a.listSleep(r.Context(), 100); databaseError(w, err) {
		return
	}
	labels, hours, err := a.sleepSeries(r.Context(), days)
	if databaseError(w, err) {
		return
	}
	d.ChartJSON = chartJSON(map[string]any{"labels": labels, "hours": hours})
	d.Summary = fmt.Sprintf("The chart shows %d days of sleep duration. It does not show a health band.", days)
	a.render(w, "sleep", d)
}
func (a *app) listSleep(ctx context.Context, limit int) ([]sleepEntry, error) {
	rows, err := a.db.QueryContext(ctx, "SELECT id,entry_date,bed_time,wake_time,duration_hours FROM sleep_entries ORDER BY entry_date DESC LIMIT ?", limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []sleepEntry
	for rows.Next() {
		var e sleepEntry
		if err := rows.Scan(&e.ID, &e.Date, &e.BedTime, &e.WakeTime, &e.DurationHours); err != nil {
			return nil, err
		}
		out = append(out, e)
	}
	return out, rows.Err()
}
func sleepDuration(bed, wake string) (float64, error) {
	return sleepDurationOn("2000-01-02", bed, wake, time.UTC)
}

func sleepDurationOn(date, bed, wake string, location *time.Location) (float64, error) {
	if !validDate(date) {
		return 0, errors.New("date is invalid")
	}
	for _, clock := range []string{bed, wake} {
		parsed, err := time.Parse("15:04", clock)
		if err != nil || parsed.Format("15:04") != clock {
			return 0, errors.New("time is invalid")
		}
	}
	bedDate := date
	if bed >= wake {
		day, _ := time.Parse("2006-01-02", date)
		bedDate = day.AddDate(0, 0, -1).Format("2006-01-02")
	}
	layout := "2006-01-02 15:04"
	b, e1 := time.ParseInLocation(layout, bedDate+" "+bed, location)
	w, e2 := time.ParseInLocation(layout, date+" "+wake, location)
	if e1 != nil || e2 != nil || b.Format(layout) != bedDate+" "+bed || w.Format(layout) != date+" "+wake {
		return 0, errors.New("time does not exist in the configured timezone")
	}
	hours := w.Sub(b).Hours()
	if hours <= 0 || hours > 24 {
		return 0, errors.New("sleep must last more than zero and at most 24 hours")
	}
	return hours, nil
}

func (a *app) saveSleep(w http.ResponseWriter, r *http.Request) {
	if err := r.ParseForm(); err != nil {
		http.Error(w, "Invalid form", 400)
		return
	}
	id, err := parseID(r)
	if err != nil {
		http.Error(w, err.Error(), 400)
		return
	}
	date := r.FormValue("entry_date")
	if !validDate(date) {
		http.Error(w, "date is invalid", 400)
		return
	}
	bed, wake := r.FormValue("bed_time"), r.FormValue("wake_time")
	duration, err := sleepDurationOn(date, bed, wake, a.location)
	if err != nil {
		http.Error(w, "Sleep times are invalid: "+err.Error(), 400)
		return
	}
	var result sql.Result
	if id > 0 {
		result, err = a.db.ExecContext(r.Context(), "UPDATE sleep_entries SET entry_date=?,bed_time=?,wake_time=?,duration_hours=? WHERE id=?", date, bed, wake, duration, id)
	} else {
		result, err = a.db.ExecContext(r.Context(), `INSERT INTO sleep_entries(entry_date,bed_time,wake_time,duration_hours) VALUES(?,?,?,?) ON CONFLICT(entry_date) DO UPDATE SET bed_time=excluded.bed_time,wake_time=excluded.wake_time,duration_hours=excluded.duration_hours`, date, bed, wake, duration)
	}
	savedRecord(w, r, result, err, "/sleep")
}
func (a *app) sleepSeries(ctx context.Context, days int) ([]string, []any, error) {
	labels := dateLabels(time.Now().In(a.location), days)
	hours := make([]any, days)
	idx := map[string]int{}
	for i, v := range labels {
		idx[v] = i
	}
	rows, err := a.db.QueryContext(ctx, "SELECT entry_date,duration_hours FROM sleep_entries WHERE entry_date>=?", labels[0])
	if err != nil {
		return labels, hours, err
	}
	defer rows.Close()
	for rows.Next() {
		var date string
		var h float64
		if err := rows.Scan(&date, &h); err != nil {
			return labels, hours, err
		}
		if i, ok := idx[date]; ok {
			hours[i] = h
		}
	}
	return labels, hours, rows.Err()
}

func (a *app) settingsPage(w http.ResponseWriter, r *http.Request) {
	d := a.baseData(r, "Settings")
	var err error
	if d.Profile, err = a.getProfile(r.Context()); databaseError(w, err) {
		return
	}
	a.render(w, "settings", d)
}
func (a *app) saveSettings(w http.ResponseWriter, r *http.Request) {
	if err := r.ParseForm(); err != nil {
		http.Error(w, "Invalid form", 400)
		return
	}
	name := strings.TrimSpace(r.FormValue("name"))
	if !validName(name) {
		http.Error(w, "name is invalid", 400)
		return
	}
	height, err := optionalFloat(r, "height_cm", 50, 300)
	if err != nil {
		http.Error(w, "height is invalid", 400)
		return
	}
	energy, err := formFloat(r, "energy_target", 500, 10000)
	if err != nil {
		http.Error(w, "energy target is invalid", 400)
		return
	}
	result, err := a.db.ExecContext(r.Context(), "UPDATE profile SET name=?,height_cm=?,energy_target=? WHERE id=1", name, height, energy)
	savedRecord(w, r, result, err, "/settings")
}

func (a *app) deletePage(kind string) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		id, err := parseID(r)
		if err != nil {
			http.NotFound(w, r)
			return
		}
		d := a.baseData(r, "Confirm deletion")
		d.DeleteKind = kind
		d.DeleteName = fmt.Sprintf("%s %d", kind, id)
		d.DeleteAction = r.URL.Path
		switch kind {
		case "food", "food entry":
			d.DeleteBack = "/nutrition"
		case "body entry":
			d.DeleteBack = "/body"
		case "sleep entry":
			d.DeleteBack = "/sleep"
		}
		a.render(w, "delete", d)
	}
}
func (a *app) deleteRecord(table, redirect string) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		id, err := parseID(r)
		if err != nil {
			http.Error(w, "invalid record", 400)
			return
		}
		allowed := map[string]bool{"foods": true, "food_entries": true, "body_entries": true, "sleep_entries": true}
		if !allowed[table] {
			http.Error(w, "invalid record", 400)
			return
		}
		result, err := a.db.ExecContext(r.Context(), "DELETE FROM "+table+" WHERE id=?", id)
		if err != nil {
			http.Error(w, "Record could not be deleted", 500)
			return
		}
		n, _ := result.RowsAffected()
		if n == 0 {
			http.NotFound(w, r)
			return
		}
		http.Redirect(w, r, redirect, 303)
	}
}

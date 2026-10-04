package main

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"
)

const sampleNutrition = `{"carbohydrate":20,"total_sugar":2,"free_sugar_percent":0,"protein":10,"fat":10,"fiber":5,"salt":0.2,"omega3":0.5,"omega6":2,"assumptions":"Cooked edible portion with olive oil. EU carbohydrate excludes fiber."}`

func fixtureNutrition(v url.Values) (food, error) {
	fields := map[string]any{"omega3": 0.0, "omega6": 0.0, "assumptions": "Test fixture."}
	for _, k := range []string{"carbohydrate", "total_sugar", "free_sugar_percent", "protein", "fat", "fiber", "salt"} {
		raw := v.Get(k)
		if k == "free_sugar_percent" && raw == "" {
			raw = "0"
		}
		value, err := strconv.ParseFloat(raw, 64)
		if err != nil {
			return food{}, err
		}
		fields[k] = value
	}
	b, err := json.Marshal(fields)
	if err != nil {
		return food{}, err
	}
	return decodeNutrition(b)
}

// Seed old tests' nutrition fixtures directly; production manual writes remain rejected.
func seedFood(t *testing.T, a *app, v url.Values, id int64) {
	t.Helper()
	f, err := fixtureNutrition(v)
	if err != nil {
		t.Fatal(err)
	}
	f.Name = v.Get("name")
	f.AnalysisModel = "fixture-model"
	f.AnalyzedAt = "2026-01-01T00:00:00Z"
	if id == 0 {
		_, err = a.db.Exec("INSERT INTO foods("+foodColumns+") VALUES(?,?,?,?,?,?,?,?,?,?,?,?,?,?)", foodArgs(f)...)
	} else {
		args := append(foodArgs(f), id)
		_, err = a.db.Exec("UPDATE foods SET name=?,carbohydrate=?,total_sugar=?,free_sugar_percent=?,protein=?,fat=?,fiber=?,salt=?,omega3=?,omega6=?,analysis_source=?,analysis_model=?,analyzed_at=?,assumptions=? WHERE id=?", args...)
	}
	if err != nil {
		t.Fatal(err)
	}
}

var draftPattern = regexp.MustCompile(`name="draft_id" value="([^"]+)"`)

func reviewToken(t *testing.T, w *httptest.ResponseRecorder) string {
	t.Helper()
	match := draftPattern.FindStringSubmatch(w.Body.String())
	if w.Code != 200 || len(match) != 2 {
		t.Fatalf("review status=%d body=%s", w.Code, w.Body.String())
	}
	return match[1]
}

func logReviewedFood(t *testing.T, a *app, path string, v url.Values) {
	t.Helper()
	if path != "/nutrition/entries" {
		var name string
		if err := a.db.QueryRow("SELECT food_name FROM food_entries WHERE id=?", strings.TrimPrefix(path, "/nutrition/entries/")).Scan(&name); err != nil {
			t.Fatal(err)
		}
		if strings.EqualFold(name, v.Get("food_name")) {
			doForm(t, a.routes(), "POST", path, v, true, 303)
			return
		}
		v = copyValues(v)
		v.Set("entry_id", strings.TrimPrefix(path, "/nutrition/entries/"))
	}
	token := reviewToken(t, request(a.routes(), "POST", "/nutrition/analyze", v.Encode(), "test"))
	doForm(t, a.routes(), "POST", path, url.Values{"csrf": {"csrf"}, "draft_id": {token}}, true, 303)
}

func copyValues(v url.Values) url.Values {
	c := url.Values{}
	for k, vs := range v {
		c[k] = append([]string(nil), vs...)
	}
	return c
}

type roundTripFunc func(*http.Request) (*http.Response, error)

func (f roundTripFunc) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }
func gatewayResponse(status int, body string) *http.Response {
	return &http.Response{StatusCode: status, Header: make(http.Header), Body: io.NopCloser(strings.NewReader(body))}
}
func completion(content string) string {
	b, _ := json.Marshal(map[string]any{"choices": []any{map[string]any{"finish_reason": "stop", "message": map[string]any{"content": content}}}})
	return string(b)
}

func mockGateway(t *testing.T, a *app, content string) *int {
	t.Helper()
	key, err := a.encryptAPIKey("test-gateway-key")
	if err != nil {
		t.Fatal(err)
	}
	execSQL(t, a.db, "UPDATE ai_settings SET base_url='https://gateway.test/v1',model='nutrition-model',api_key=? WHERE id=1", key)
	calls := new(int)
	a.aiTransport = roundTripFunc(func(r *http.Request) (*http.Response, error) {
		*calls++
		if r.URL.String() != "https://gateway.test/v1/chat/completions" || r.Header.Get("Authorization") != "Bearer test-gateway-key" {
			t.Fatalf("wrong gateway request: %s", r.URL)
		}
		var p struct {
			Model          string
			Messages       []struct{ Role, Content string }
			ResponseFormat map[string]any `json:"response_format"`
		}
		if err := json.NewDecoder(r.Body).Decode(&p); err != nil {
			t.Fatal(err)
		}
		if p.Model != "nutrition-model" || len(p.Messages) != 2 || p.Messages[0].Role != "system" || p.Messages[1].Role != "user" || !strings.Contains(p.Messages[0].Content, "Spain") {
			t.Fatalf("wrong model or messages: %+v", p)
		}
		format, _ := json.Marshal(p.ResponseFormat)
		if strings.Contains(p.Messages[0].Content, "omega3_to_omega6") || bytes.Contains(format, []byte("omega3_to_omega6")) {
			t.Fatal("AI request asks the model to calculate the omega ratio")
		}
		// The database remains usable while the upstream request is active.
		var count int
		if err := a.db.QueryRow("SELECT COUNT(*) FROM foods").Scan(&count); err != nil {
			t.Fatal(err)
		}
		return gatewayResponse(200, completion(content)), nil
	})
	return calls
}

func TestAIReviewSaveReuseAndHistoricalSnapshot(t *testing.T) {
	a := testApp(t)
	calls := mockGateway(t, a, sampleNutrition)
	h := a.routes()
	v := entryForm()
	v.Set("food_name", "Lentejas cocidas con aceite de oliva")
	v.Set("consumed_g", "150")
	w := request(h, "POST", "/nutrition/analyze", v.Encode(), "test")
	token := reviewToken(t, w)
	for _, want := range []string{"30.00 g", "0.75 g", "3.00 g", "0.25", "Cooked edible portion"} {
		if !strings.Contains(w.Body.String(), want) {
			t.Fatalf("missing review value %q", want)
		}
	}
	var count int
	a.db.QueryRow("SELECT COUNT(*) FROM foods").Scan(&count)
	if count != 0 {
		t.Fatal("analysis saved a food before review")
	}
	save := url.Values{"csrf": {"csrf"}, "draft_id": {token}}
	doForm(t, h, "POST", "/nutrition/entries", save, true, 303)
	doForm(t, h, "POST", "/nutrition/entries", save, true, 303)
	a.db.QueryRow("SELECT COUNT(*) FROM food_entries").Scan(&count)
	if count != 1 {
		t.Fatal("repeat save duplicated entry")
	}
	totals := mustDailyNutrition(t, a.db, "2026-01-01")
	if totals.Carbohydrate != 30 || totals.Omega3 == nil || *totals.Omega3 != 0.75 || *totals.Omega6 != 3 {
		t.Fatalf("portion totals: %+v", totals)
	}
	v.Set("food_name", "  Lentejas cocidas con aceite de oliva  ")
	v.Set("consumed_g", "200")
	reviewToken(t, request(h, "POST", "/nutrition/analyze", v.Encode(), "test"))
	if *calls != 1 {
		t.Fatal("saved analysis was not reused")
	}
	token = reviewToken(t, request(h, "POST", "/nutrition/foods/1/analyze", url.Values{"csrf": {"csrf"}}.Encode(), "test"))
	doForm(t, h, "POST", "/nutrition/foods/1", url.Values{"csrf": {"csrf"}, "draft_id": {token}}, true, 303)
	if *calls != 2 {
		t.Fatal("reanalysis did not call model")
	}
	execSQL(t, a.db, "UPDATE foods SET carbohydrate=10,omega3=1 WHERE id=1")
	totals = mustDailyNutrition(t, a.db, "2026-01-01")
	if totals.Carbohydrate != 30 || *totals.Omega3 != 0.75 {
		t.Fatal("library update changed history")
	}
	v.Set("consumed_g", "200")
	doForm(t, h, "POST", "/nutrition/entries/1", v, true, 303)
	totals = mustDailyNutrition(t, a.db, "2026-01-01")
	if totals.Carbohydrate != 40 || *totals.Omega3 != 1 {
		t.Fatal("weight edit changed snapshot")
	}
	doForm(t, h, "POST", "/nutrition/foods/1/delete", url.Values{"csrf": {"csrf"}}, true, 303)
	doForm(t, h, "POST", "/nutrition/entries/1", v, true, 303)
}

func TestAIDraftsBoundToSessionExpireAndRejectTampering(t *testing.T) {
	a := testApp(t)
	mockGateway(t, a, sampleNutrition)
	h := a.routes()
	v := entryForm()
	token := reviewToken(t, request(h, "POST", "/nutrition/analyze", v.Encode(), "test"))
	save := url.Values{"csrf": {"csrf"}, "draft_id": {token}, "omega3": {"100"}}
	doForm(t, h, "POST", "/nutrition/entries", save, true, 400)
	save.Del("omega3")
	save.Set("omega3_to_omega6", "4")
	doForm(t, h, "POST", "/nutrition/entries", save, true, 400)
	save.Del("omega3_to_omega6")
	a.sessions.m["other"] = session{csrf: "csrf", expires: time.Now().Add(time.Hour)}
	w := request(h, "POST", "/nutrition/entries", save.Encode(), "other")
	if w.Code != 409 {
		t.Fatal("cross-session draft accepted")
	}
	a.drafts.m[token].Expires = time.Now().Add(-time.Second)
	doForm(t, h, "POST", "/nutrition/entries", save, true, 409)
	for _, path := range []string{"/nutrition/foods", "/nutrition/entries", "/nutrition/analyze"} {
		doForm(t, h, "POST", path, foodForm(), true, 400)
	}
	var count int
	a.db.QueryRow("SELECT COUNT(*) FROM foods").Scan(&count)
	if count != 0 {
		t.Fatal("rejected writes created foods")
	}
}

func TestAIDraftConcurrentSavesAndConflicts(t *testing.T) {
	a := testApp(t)
	mockGateway(t, a, sampleNutrition)
	token := reviewToken(t, request(a.routes(), "POST", "/nutrition/analyze", entryForm().Encode(), "test"))
	save := url.Values{"csrf": {"csrf"}, "draft_id": {token}}
	var wg sync.WaitGroup
	for range 8 {
		wg.Go(func() {
			w := request(a.routes(), "POST", "/nutrition/entries", save.Encode(), "test")
			if w.Code != 303 {
				t.Errorf("save status %d", w.Code)
			}
		})
	}
	wg.Wait()
	var count int
	a.db.QueryRow("SELECT COUNT(*) FROM food_entries").Scan(&count)
	if count != 1 {
		t.Fatal("concurrent saves duplicated food entry")
	}
	token = reviewToken(t, request(a.routes(), "POST", "/nutrition/analyze", entryForm().Encode(), "test"))
	execSQL(t, a.db, "UPDATE foods SET protein=12 WHERE id=1")
	doForm(t, a.routes(), "POST", "/nutrition/entries", url.Values{"csrf": {"csrf"}, "draft_id": {token}}, true, 409)
}

func TestAILegacyRequiresReviewAndPreservesPastValues(t *testing.T) {
	a := testApp(t)
	calls := mockGateway(t, a, sampleNutrition)
	execSQL(t, a.db, "INSERT INTO foods(name,carbohydrate,total_sugar,protein,fat,fiber,salt) VALUES('Oats',60,1,12,7,10,0)")
	execSQL(t, a.db, `INSERT INTO food_entries(food_id,entry_date,consumed_g,food_name,carbohydrate,total_sugar,protein,fat,fiber,salt) VALUES(1,'2026-01-01',100,'Oats',60,1,12,7,10,0)`)
	token := reviewToken(t, request(a.routes(), "POST", "/nutrition/analyze", entryForm().Encode(), "test"))
	if *calls != 1 {
		t.Fatal("legacy food bypassed AI")
	}
	totals := mustDailyNutrition(t, a.db, "2026-01-01")
	if totals.Omega3 != nil {
		t.Fatal("legacy omega fabricated")
	}
	doForm(t, a.routes(), "POST", "/nutrition/entries", url.Values{"csrf": {"csrf"}, "draft_id": {token}}, true, 303)
	totals = mustDailyNutrition(t, a.db, "2026-01-01")
	if totals.Carbohydrate != 80 || totals.Omega3 != nil {
		t.Fatalf("legacy history changed: %+v", totals)
	}
}

func TestAINutritionValidationAndZeroRatios(t *testing.T) {
	for _, tc := range []struct{ name, old, new string }{
		{"missing omega", `,"omega3":0.5`, ""}, {"null omega", `"omega3":0.5`, `"omega3":null`}, {"sugar", `"total_sugar":2`, `"total_sugar":21`}, {"fat", `"fat":10`, `"fat":1`}, {"negative", `"omega6":2`, `"omega6":-1`}, {"NaN", `"protein":10`, `"protein":NaN`}, {"range", `"protein":10`, `"protein":101`}, {"extra", `"salt":0.2`, `"bogus":3,"salt":0.2`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if _, err := decodeNutrition([]byte(strings.Replace(sampleNutrition, tc.old, tc.new, 1))); err == nil {
				t.Fatal("invalid nutrition accepted")
			}
		})
	}
	for _, tc := range []struct {
		o3, o6 float64
		want   string
	}{{0, 0, "No omega fats"}, {1, 0, "Undefined (omega 6 is zero)"}, {0, 1, "0.00"}, {0.5, 2, "0.25"}} {
		var fields map[string]any
		json.Unmarshal([]byte(sampleNutrition), &fields)
		fields["omega3"], fields["omega6"] = tc.o3, tc.o6
		b, _ := json.Marshal(fields)
		f, err := decodeNutrition(b)
		if err != nil || omegaRatio(f.Omega3, f.Omega6) != tc.want {
			t.Fatalf("zero ratios: %+v %v", f, err)
		}
	}
	if validDescription(strings.Repeat("🍎", 1001)) || !validDescription(strings.Repeat("é", 2000)) || validDescription("invalid\xff") {
		t.Fatal("description limit does not match UTF16")
	}
}

func TestAIReviewIgnoresModelRatio(t *testing.T) {
	for _, ratio := range []string{"4", "null", `"not a ratio"`} {
		t.Run(ratio, func(t *testing.T) {
			a := testApp(t)
			content := strings.Replace(sampleNutrition, `"omega6":2`, `"omega6":2,"omega3_to_omega6":`+ratio, 1)
			mockGateway(t, a, content)
			v := entryForm()
			v.Set("consumed_g", "150")
			w := request(a.routes(), "POST", "/nutrition/analyze", v.Encode(), "test")
			token := reviewToken(t, w)
			if !strings.Contains(w.Body.String(), "Omega 3 ÷ omega 6 ratio: 0.25") {
				t.Fatal("review did not calculate the ratio from the omega amounts")
			}
			doForm(t, a.routes(), "POST", "/nutrition/entries", url.Values{"csrf": {"csrf"}, "draft_id": {token}}, true, 303)
			totals := mustDailyNutrition(t, a.db, "2026-01-01")
			if totals.Omega3 == nil || totals.Omega6 == nil || *totals.Omega3 != 0.75 || *totals.Omega6 != 3 || omegaRatio(totals.Omega3, totals.Omega6) != "0.25" {
				t.Fatal("saved ratio did not use the scaled omega amounts")
			}
		})
	}
}

func TestAIDailyRatioUsesAmounts(t *testing.T) {
	a := testApp(t)
	execSQL(t, a.db, `INSERT INTO food_entries(entry_date,consumed_g,food_name,carbohydrate,total_sugar,protein,fat,fiber,salt,omega3,omega6) VALUES('2026-01-01',100,'Fish',0,0,10,10,0,0,2,1),('2026-01-01',200,'Oil',0,0,0,100,0,0,1,10)`)
	totals := mustDailyNutrition(t, a.db, "2026-01-01")
	if *totals.Omega3 != 4 || *totals.Omega6 != 21 || omegaRatio(totals.Omega3, totals.Omega6) != "0.19" {
		t.Fatalf("daily amounts %+v", totals)
	}
	execSQL(t, a.db, "UPDATE food_entries SET omega6=NULL WHERE food_name='Oil'")
	totals = mustDailyNutrition(t, a.db, "2026-01-01")
	if totals.Omega3 != nil || totals.Omega6 != nil {
		t.Fatal("incomplete totals presented as complete")
	}
}

func TestAIOmegaRatioHistoryUsesPortionsAndLeavesGaps(t *testing.T) {
	a := testApp(t)
	execSQL(t, a.db, `INSERT INTO food_entries(entry_date,consumed_g,food_name,carbohydrate,total_sugar,protein,fat,fiber,salt,omega3,omega6) VALUES
('2026-01-01',100,'Fish',0,0,10,10,0,0,2,1),
('2026-01-01',200,'Oil',0,0,0,100,0,0,1,10),
('2026-01-02',100,'Known food',0,0,0,10,0,0,1,2),
('2026-01-02',100,'Legacy food',0,0,0,10,0,0,NULL,NULL),
('2026-01-03',100,'Only omega 6',0,0,0,10,0,0,0,2),
('2026-01-04',100,'Only omega 3',0,0,0,10,0,0,1,0),
('2026-01-05',100,'No omega fats',0,0,0,0,0,0,0,0),
('2025-12-31',100,'Outside range',0,0,0,10,0,0,1,2)`)
	ratios, err := a.omegaRatioSeries(context.Background(), []string{"2026-01-01", "2026-01-02", "2026-01-03", "2026-01-04", "2026-01-05", "2026-01-06"})
	if err != nil || len(ratios) != 6 || ratios[0] != 4.0/21 || ratios[2] != 0.0 {
		t.Fatalf("incorrect portion ratios: %v, %v", ratios, err)
	}
	for _, i := range []int{1, 3, 4, 5} {
		if ratios[i] != nil {
			t.Fatalf("day %d should be a gap: %v", i, ratios[i])
		}
	}
}

func TestAIReviewFormatsTwoDecimalsWithoutRoundingStoredNutrition(t *testing.T) {
	a := testApp(t)
	var values map[string]any
	if err := json.Unmarshal([]byte(sampleNutrition), &values); err != nil {
		t.Fatal(err)
	}
	values["carbohydrate"] = 20.123456
	values["omega3"] = 0.56789
	values["omega6"] = 2.34567
	content, _ := json.Marshal(values)
	mockGateway(t, a, string(content))
	v := entryForm()
	v.Set("consumed_g", "150")
	w := request(a.routes(), "POST", "/nutrition/analyze", v.Encode(), "test")
	for _, want := range []string{"30.19 g", "20.12 g", "0.85 g", "0.57 g", "3.52 g", "2.35 g", "0.24"} {
		if !strings.Contains(w.Body.String(), want) {
			t.Fatalf("missing formatted value %q", want)
		}
	}
	doForm(t, a.routes(), "POST", "/nutrition/entries", url.Values{"csrf": {"csrf"}, "draft_id": {reviewToken(t, w)}}, true, 303)
	var carbs, omega3, omega6 float64
	if err := a.db.QueryRow("SELECT carbohydrate,omega3,omega6 FROM food_entries").Scan(&carbs, &omega3, &omega6); err != nil {
		t.Fatal(err)
	}
	if carbs != 20.123456 || omega3 != 0.56789 || omega6 != 2.34567 {
		t.Fatal("display rounding changed saved nutrition")
	}
}

func TestAIGatewayFallbackAndFailures(t *testing.T) {
	a := testApp(t)
	mockGateway(t, a, sampleNutrition)
	calls := 0
	a.aiTransport = roundTripFunc(func(r *http.Request) (*http.Response, error) {
		calls++
		var p map[string]any
		json.NewDecoder(r.Body).Decode(&p)
		if calls == 1 {
			if p["response_format"] == nil {
				t.Fatal("schema omitted")
			}
			return gatewayResponse(400, `{"error":{"param":"response_format","message":"json_schema is not supported"}}`), nil
		}
		if p["response_format"] != nil {
			t.Fatal("fallback still sends unsupported parameter")
		}
		content := strings.Replace(sampleNutrition, `"omega6":2`, `"omega6":2,"omega3_to_omega6":4`, 1)
		return gatewayResponse(200, completion(content)), nil
	})
	if _, err := a.analyzeNutrition(context.Background(), foodInput{Description: "Oats", Grams: 100}); err != nil || calls != 2 {
		t.Fatalf("fallback %v calls=%d", err, calls)
	}
	for _, tc := range []struct {
		name    string
		status  int
		body    string
		network bool
	}{
		{"bad JSON", 200, "garbage", false}, {"missing omega", 200, completion(strings.Replace(sampleNutrition, `"omega3":0.5`, `"omega3":null`, 1)), false}, {"refusal", 200, `{"choices":[{"finish_reason":"stop","message":{"refusal":"No"}}]}`, false}, {"incomplete", 200, `{"choices":[{"finish_reason":"length","message":{"content":"{}"}}]}`, false}, {"oversized", 200, strings.Repeat("x", (128<<10)+1), false}, {"auth", 401, "test-gateway-key", false}, {"limit", 429, "test-gateway-key", false}, {"redirect", 302, "test-gateway-key", false}, {"network", 0, "", true}, {"wrong model", 400, `{"error":{"param":"model","message":"model unsupported"}}`, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			calls := 0
			a.aiTransport = roundTripFunc(func(*http.Request) (*http.Response, error) {
				calls++
				if tc.network {
					return nil, errors.New("test-gateway-key")
				}
				return gatewayResponse(tc.status, tc.body), nil
			})
			v := entryForm()
			v.Set("food_name", "Arroz cocido con aceite")
			v.Set("consumed_g", "125")
			w := request(a.routes(), "POST", "/nutrition/analyze", v.Encode(), "test")
			if w.Code != 400 || calls != 1 || !strings.Contains(w.Body.String(), "Arroz cocido con aceite") || !strings.Contains(w.Body.String(), `value="125.00"`) || strings.Contains(w.Body.String(), "test-gateway-key") {
				t.Fatalf("failure handling status=%d calls=%d body=%s", w.Code, calls, w.Body.String())
			}
		})
	}
}

func TestAIResponseFormatRejections(t *testing.T) {
	for _, tc := range []struct {
		name   string
		status int
		body   string
		want   bool
	}{
		{"unsupported parameter", 400, `{"error":{"param":"response_format","code":"unsupported_parameter","message":"Unsupported parameter"}}`, true},
		{"DeepSeek enum", 400, `{"error":{"param":null,"code":"invalid_request_error","message":"Failed to deserialize the JSON body into the target type: response_format.type: unknown variant ` + "`json_schema`" + `, expected ` + "`text` or `json_object`" + ` at line 1 column 50"}}`, true},
		{"allowed types", 422, `{"error":{"param":"response_format.type","message":"Invalid value: json_schema. Must be one of text or json_object."}}`, true},
		{"literal types", 422, `{"error":{"param":"response_format.type","message":"Input should be 'text' or 'json_object', received 'json_schema'"}}`, true},
		{"top-level message", 400, `{"message":"response_format json_schema is not supported","code":400}`, true},
		{"string error", 400, `{"error":"response_format.type: unknown variant 'json_schema', expected 'text' or 'json_object'"}`, true},
		{"string detail", 422, `{"detail":"response_format json_schema is not supported"}`, true},
		{"numeric error code", 400, `{"error":{"message":"json_schema is not supported","code":400}}`, true},
		{"unavailable format type", 400, `{"error":{"message":"This response_format type is unavailable now (request_id: d4b727f4-024e-483d-969b-9dc8381b9d17)","type":"invalid_request_error"}}`, true},
		{"format type not available", 400, `{"error":{"message":"This response_format type is not available now"}}`, true},
		{"invalid schema", 400, `{"error":{"param":"response_format","message":"Invalid schema for response_format 'food_nutrition': additionalProperties must be false"}}`, false},
		{"wrong model", 400, `{"error":{"param":"model","message":"Unsupported model"}}`, false},
		{"unavailable model", 400, `{"error":{"param":"model","message":"Model unavailable now; response_format is valid"}}`, false},
		{"authentication", 401, `{"error":{"param":"response_format","message":"json_schema unsupported"}}`, false},
		{"rate limit", 429, `{"error":{"param":"response_format","message":"json_schema unsupported"}}`, false},
		{"malformed", 400, "unknown variant json_schema, expected json_object", false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := unsupportedResponseFormat(tc.status, []byte(tc.body)); got != tc.want {
				t.Fatalf("fallback detection = %v, want %v", got, tc.want)
			}
		})
	}
}

func TestAIGatewayUnavailableFormatRetriesOnce(t *testing.T) {
	const rejection = `{"error":{"message":"This response_format type is unavailable now (request_id: d4b727f4-024e-483d-969b-9dc8381b9d17)","type":"invalid_request_error"}}`
	for _, tc := range []struct {
		name, content string
		status        int
		want          string
	}{
		{"valid estimates", sampleNutrition, 200, "Connection succeeded"},
		{"missing omega", strings.Replace(sampleNutrition, `"omega3":0.5`, `"omega3":null`, 1), 400, "including omega 3 and omega 6"},
		{"second rejection", "", 400, "Gateway message: This response_format type is unavailable now"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			a := testApp(t)
			mockGateway(t, a, sampleNutrition)
			execSQL(t, a.db, "UPDATE ai_settings SET base_url='https://deepseek.dait.es/v1',model='deepseek-flash'")
			calls := 0
			var originalMessages json.RawMessage
			a.aiTransport = roundTripFunc(func(r *http.Request) (*http.Response, error) {
				calls++
				var p struct {
					Model          string
					Messages       json.RawMessage
					ResponseFormat map[string]any `json:"response_format"`
				}
				if err := json.NewDecoder(r.Body).Decode(&p); err != nil {
					t.Fatal(err)
				}
				if r.URL.String() != "https://deepseek.dait.es/v1/chat/completions" || p.Model != "deepseek-flash" || r.Header.Get("Authorization") != "Bearer test-gateway-key" {
					t.Fatal("retry changed the configured gateway, model, or credentials")
				}
				if calls == 1 {
					if p.ResponseFormat["type"] != "json_schema" {
						t.Fatal("initial request omitted strict schema")
					}
					originalMessages = p.Messages
					return gatewayResponse(400, rejection), nil
				}
				if calls != 2 || p.ResponseFormat != nil || !bytes.Equal(originalMessages, p.Messages) {
					t.Fatal("retry must preserve instructions and omit response_format exactly once")
				}
				if tc.content == "" {
					return gatewayResponse(400, rejection), nil
				}
				// DeepSeek includes reasoning and usage fields alongside the final
				// assistant content. Only the final content supplies nutrition.
				content, _ := json.Marshal(tc.content)
				return gatewayResponse(200, `{"id":"1b79b725-72eb-43af-aba6-b3cb43759888","object":"chat.completion","model":"deepseek-flash","choices":[{"index":0,"message":{"role":"assistant","content":`+string(content)+`,"reasoning_content":"Estimate the requested EU nutrients."},"logprobs":null,"finish_reason":"stop"}],"usage":{"prompt_tokens":31,"completion_tokens":44,"total_tokens":75}}`), nil
			})
			w := request(a.routes(), "POST", "/settings/ai/test", url.Values{"csrf": {"csrf"}}.Encode(), "test")
			if w.Code != tc.status || calls != 2 || !strings.Contains(w.Body.String(), tc.want) {
				t.Fatalf("retry status=%d calls=%d body=%s", w.Code, calls, w.Body.String())
			}
			var count int
			if err := a.db.QueryRow("SELECT (SELECT COUNT(*) FROM foods)+(SELECT COUNT(*) FROM food_entries)").Scan(&count); err != nil || count != 0 {
				t.Fatal("connection test changed nutrition data")
			}
		})
	}
}

func TestAIGatewayRejectionDetailsAndSecretMasking(t *testing.T) {
	for _, tc := range []struct {
		name, body, want string
	}{
		{"routing", `{"error":{"type":"invalid_request_error","message":"No provider available for model deepseek-flash"}}`, "No provider available for model deepseek-flash"},
		{"top-level", `{"message":"The model is not enabled for Chat Completions","code":400}`, "The model is not enabled for Chat Completions"},
		{"error string", `{"error":"No matching provider"}`, "No matching provider"},
		{"saved key", `{"error":{"message":"Rejected value test-gateway-key"}}`, "Rejected value [redacted]"},
		{"upstream key", `{"error":{"message":"Upstream API key upstream-private-value is invalid"}}`, "The gateway reported a credential error"},
		{"authorization", `{"error":{"message":"Authorization: Bearer upstream-private-value is invalid"}}`, "The gateway reported a credential error"},
		{"HTML", `{"error":{"message":"<script>alert('gateway')</script>"}}`, "&lt;script&gt;"},
		{"non-JSON", "test-gateway-key", "The AI service returned HTTP 400. No data was saved."},
	} {
		t.Run(tc.name, func(t *testing.T) {
			a := testApp(t)
			mockGateway(t, a, sampleNutrition)
			calls := 0
			a.aiTransport = roundTripFunc(func(*http.Request) (*http.Response, error) {
				calls++
				return gatewayResponse(400, tc.body), nil
			})
			w := request(a.routes(), "POST", "/settings/ai/test", url.Values{"csrf": {"csrf"}}.Encode(), "test")
			body := w.Body.String()
			if w.Code != 400 || calls != 1 || !strings.Contains(body, "Connection test failed.") || !strings.Contains(body, tc.want) {
				t.Fatalf("connection rejection status=%d calls=%d body=%s", w.Code, calls, body)
			}
			for _, secret := range []string{"test-gateway-key", "upstream-private-value", "<script>"} {
				if strings.Contains(body, secret) {
					t.Fatalf("unsafe gateway content in page: %s", secret)
				}
			}
			var count int
			if err := a.db.QueryRow("SELECT (SELECT COUNT(*) FROM foods)+(SELECT COUNT(*) FROM food_entries)").Scan(&count); err != nil || count != 0 {
				t.Fatal("failed connection test changed nutrition data")
			}
		})
	}
	// Mask the complete credential before shortening a long response. Encoded
	// credentials and control characters must not escape the same protection.
	key := "private/key?with=encoding"
	for _, echoed := range []string{key, url.QueryEscape(key), url.PathEscape(key)} {
		data, _ := json.Marshal(map[string]string{"message": strings.Repeat("x", 390) + echoed})
		got := safeGatewayMessage(data, key)
		if strings.Contains(got, "private") || len([]rune(got)) > 401 {
			t.Fatal("long error leaks a partial credential")
		}
	}
	data := []byte(`{"message":"No\nprovider\u0000\u202eavailable"}`)
	if got := safeGatewayMessage(data, ""); got != "No provider available" {
		t.Fatalf("control characters in error: %q", got)
	}
}

func TestAIGatewayWrappedFormatRejectionStillValidatesRetry(t *testing.T) {
	a := testApp(t)
	mockGateway(t, a, sampleNutrition)
	calls := 0
	a.aiTransport = roundTripFunc(func(r *http.Request) (*http.Response, error) {
		calls++
		var p map[string]any
		if err := json.NewDecoder(r.Body).Decode(&p); err != nil {
			t.Fatal(err)
		}
		if calls == 1 {
			return gatewayResponse(400, `{"message":"json_schema is not supported for response_format","code":400}`), nil
		}
		if calls != 2 || p["response_format"] != nil {
			t.Fatal("invalid format retry")
		}
		return gatewayResponse(200, completion(strings.Replace(sampleNutrition, `"omega3":0.5`, `"omega3":null`, 1))), nil
	})
	v := entryForm()
	v.Set("food_name", "Arroz cocido con aceite")
	v.Set("consumed_g", "125")
	w := request(a.routes(), "POST", "/nutrition/analyze", v.Encode(), "test")
	if w.Code != 400 || calls != 2 || !strings.Contains(w.Body.String(), "including omega 3 and omega 6") || !strings.Contains(w.Body.String(), "Arroz cocido con aceite") || !strings.Contains(w.Body.String(), `value="125.00"`) {
		t.Fatalf("invalid retry was not rejected: %d %s", w.Code, w.Body.String())
	}
	var count int
	if err := a.db.QueryRow("SELECT (SELECT COUNT(*) FROM foods)+(SELECT COUNT(*) FROM food_entries)").Scan(&count); err != nil || count != 0 {
		t.Fatal("invalid retry saved nutrition data")
	}
}

func TestAIDeepSeekRejectionRetriesAndSavesReview(t *testing.T) {
	a := testApp(t)
	mockGateway(t, a, sampleNutrition)
	execSQL(t, a.db, "UPDATE ai_settings SET base_url='https://deepseek.dait.es/v1',model='deepseek-flash'")
	calls := 0
	var messages any
	a.aiTransport = roundTripFunc(func(r *http.Request) (*http.Response, error) {
		calls++
		var p map[string]any
		if err := json.NewDecoder(r.Body).Decode(&p); err != nil {
			t.Fatal(err)
		}
		if r.URL.String() != "https://deepseek.dait.es/v1/chat/completions" || p["model"] != "deepseek-flash" {
			t.Fatal("gateway address or model changed")
		}
		if calls == 1 {
			if p["response_format"].(map[string]any)["type"] != "json_schema" {
				t.Fatal("strict schema missing from initial request")
			}
			messages = p["messages"]
			return gatewayResponse(400, `{"error":{"message":"Failed to deserialize the JSON body into the target type: response_format.type: unknown variant 'json_schema', expected 'text' or 'json_object'","type":"invalid_request_error","param":null,"code":"invalid_request_error"}}`), nil
		}
		original, _ := json.Marshal(messages)
		retry, _ := json.Marshal(p["messages"])
		if calls != 2 || p["response_format"] != nil || !bytes.Equal(original, retry) {
			t.Fatal("retry must preserve JSON instructions and omit the rejected format")
		}
		return gatewayResponse(200, completion(sampleNutrition)), nil
	})
	v := entryForm()
	v.Set("food_name", "Arroz cocido con aceite")
	v.Set("consumed_g", "125")
	w := request(a.routes(), "POST", "/nutrition/analyze", v.Encode(), "test")
	if w.Code != 200 || calls != 2 {
		t.Fatalf("review status=%d calls=%d body=%s", w.Code, calls, w.Body.String())
	}
	doForm(t, a.routes(), "POST", "/nutrition/entries", url.Values{"csrf": {"csrf"}, "draft_id": {reviewToken(t, w)}}, true, 303)
	var omega3, omega6, grams float64
	var model string
	if err := a.db.QueryRow("SELECT omega3,omega6,consumed_g,analysis_model FROM food_entries").Scan(&omega3, &omega6, &grams, &model); err != nil {
		t.Fatal(err)
	}
	if omega3 != 0.5 || omega6 != 2 || grams != 125 || model != "deepseek-flash" || calls != 2 {
		t.Fatal("review was not saved from the validated retry")
	}
}

func TestAISettingsEncryptionPersistenceAndSecretMasking(t *testing.T) {
	a := testApp(t)
	h := a.routes()
	v := url.Values{"csrf": {"csrf"}, "base_url": {"https://deepseek.dait.es/v1/"}, "model": {"deepseek-model"}, "api_key": {"private-test-key"}}
	doForm(t, h, "POST", "/settings/ai", v, true, 303)
	s, err := a.getAISettings(context.Background())
	if err != nil || bytes.Contains(s.KeyCipher, []byte("private-test-key")) {
		t.Fatal("key not encrypted")
	}
	info, err := os.Stat(filepath.Join(filepath.Dir(a.cfg.dbPath), "ai-secret.key"))
	if err != nil || info.Mode().Perm() != 0600 {
		t.Fatal("encryption file permissions")
	}
	key, err := a.decryptAPIKey(s.KeyCipher)
	if err != nil || key != "private-test-key" {
		t.Fatal("key cannot be read after save")
	}
	reopened := &app{cfg: a.cfg}
	key, err = reopened.decryptAPIKey(s.KeyCipher)
	if err != nil || key != "private-test-key" {
		t.Fatal("restart lost key")
	}
	w := request(h, "GET", "/settings", "", "test")
	if strings.Contains(w.Body.String(), "private-test-key") || strings.Contains(w.Body.String(), string(s.KeyCipher)) {
		t.Fatal("settings expose secret")
	}
	v.Set("api_key", "")
	v.Set("model", "replacement-model")
	doForm(t, h, "POST", "/settings/ai", v, true, 303)
	s, _ = a.getAISettings(context.Background())
	key, _ = a.decryptAPIKey(s.KeyCipher)
	if key != "private-test-key" || s.Model != "replacement-model" {
		t.Fatal("blank field did not preserve key")
	}
	path := filepath.Join(filepath.Dir(a.cfg.dbPath), "ai-secret.key")
	os.Remove(path)
	w = request(h, "GET", "/settings", "", "test")
	if !strings.Contains(w.Body.String(), "replacement API key") {
		t.Fatal("missing encryption file not reported")
	}
	doForm(t, h, "POST", "/settings/ai", v, true, 400)
	if _, err := os.Stat(path); !os.IsNotExist(err) {
		t.Fatal("missing key file silently regenerated")
	}
	v.Set("api_key", "new-private-key")
	doForm(t, h, "POST", "/settings/ai", v, true, 303)
	s, _ = a.getAISettings(context.Background())
	key, _ = a.decryptAPIKey(s.KeyCipher)
	if key != "new-private-key" {
		t.Fatal("explicit replacement failed")
	}
	v.Set("api_key", "")
	v.Set("clear_key", "true")
	doForm(t, h, "POST", "/settings/ai", v, true, 303)
	s, _ = a.getAISettings(context.Background())
	if s.HasKey {
		t.Fatal("clear action did not clear key")
	}
	for _, raw := range []string{"http://gateway.test/v1", "https://user:password@gateway.test/v1", "https://gateway.test/v1?key=x", "https://gateway.test/v1#key", "https://gateway.test/v1/chat/completions"} {
		if _, err := validateAIAddress(raw); err == nil {
			t.Fatalf("invalid base accepted %s", raw)
		}
	}
}

func TestAIAnalysisRoutesRequireAuthenticationAndCSRF(t *testing.T) {
	a := testApp(t)
	for _, path := range []string{"/nutrition/analyze", "/nutrition/foods/1/analyze", "/settings/ai", "/settings/ai/test"} {
		if w := request(a.routes(), "POST", path, "csrf=csrf", ""); w.Code != 303 {
			t.Fatalf("unauthenticated %s %d", path, w.Code)
		}
		if w := request(a.routes(), "POST", path, "csrf=wrong", "test"); w.Code != 403 {
			t.Fatalf("missing CSRF %s %d", path, w.Code)
		}
	}
}

func TestAIConnectionExercisesNutritionContract(t *testing.T) {
	a := testApp(t)
	calls := mockGateway(t, a, sampleNutrition)
	w := request(a.routes(), "POST", "/settings/ai/test", url.Values{"csrf": {"csrf"}}.Encode(), "test")
	if w.Code != 200 || *calls != 1 || !strings.Contains(w.Body.String(), `<p class="notice" role="status">Connection succeeded`) || strings.Contains(w.Body.String(), `class="alert"`) {
		t.Fatalf("test connection: %d %s", w.Code, w.Body.String())
	}
	var n int
	a.db.QueryRow("SELECT COUNT(*) FROM foods").Scan(&n)
	if n != 0 {
		t.Fatal("connection test saved food")
	}
}

func TestAIMigrationPreservesLegacyDatabase(t *testing.T) {
	a := testApp(t)
	var trigger int
	if err := a.db.QueryRow("SELECT COUNT(*) FROM sqlite_master WHERE type='trigger' AND name='foods_sync_entries'").Scan(&trigger); err != nil || trigger != 0 {
		t.Fatal("history synchronization trigger remains")
	}
	if err := migrate(a.db); err != nil {
		t.Fatal(err)
	}
	var migrations int
	a.db.QueryRow("SELECT COUNT(*) FROM schema_migrations").Scan(&migrations)
	if migrations != 6 {
		t.Fatal("migration not applied exactly once")
	}
}

func TestAIUpgradePreservesExistingManualHistory(t *testing.T) {
	db, err := sql.Open("sqlite", filepath.Join(t.TempDir(), "legacy.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	execSQL(t, db, "PRAGMA foreign_keys=ON; CREATE TABLE schema_migrations(version TEXT PRIMARY KEY,applied_at TEXT NOT NULL DEFAULT CURRENT_TIMESTAMP)")
	for _, name := range []string{"001_initial", "002_fat", "003_sync_food_entries", "004_remove_free_sugar", "005_free_sugar_percentage"} {
		body, err := files.ReadFile("migrations/" + name + ".sql")
		if err != nil {
			t.Fatal(err)
		}
		execSQL(t, db, string(body))
		execSQL(t, db, "INSERT INTO schema_migrations(version) VALUES(?)", name)
	}
	execSQL(t, db, "INSERT INTO foods(name,carbohydrate,total_sugar,protein,fat,fiber,salt) VALUES('Oats',60,1,12,7,10,0)")
	execSQL(t, db, `INSERT INTO food_entries(food_id,entry_date,consumed_g,food_name,carbohydrate,total_sugar,protein,fat,fiber,salt) VALUES(1,'2026-01-01',100,'Oats',60,1,12,7,10,0)`)
	if err := migrate(db); err != nil {
		t.Fatal(err)
	}
	execSQL(t, db, "UPDATE foods SET carbohydrate=30 WHERE id=1")
	var carbs float64
	var omega *float64
	var source string
	if err := db.QueryRow("SELECT carbohydrate,omega3,analysis_source FROM food_entries WHERE id=1").Scan(&carbs, &omega, &source); err != nil || carbs != 60 || omega != nil || source != "manual" {
		t.Fatalf("upgrade changed history: %g %v %s %v", carbs, omega, source, err)
	}
}

func TestAISaveRollsBackFoodWhenEntryWriteFails(t *testing.T) {
	a := testApp(t)
	mockGateway(t, a, sampleNutrition)
	token := reviewToken(t, request(a.routes(), "POST", "/nutrition/analyze", entryForm().Encode(), "test"))
	execSQL(t, a.db, `CREATE TRIGGER fail_entry BEFORE INSERT ON food_entries BEGIN SELECT RAISE(ABORT,'test write failure'); END`)
	w := request(a.routes(), "POST", "/nutrition/entries", url.Values{"csrf": {"csrf"}, "draft_id": {token}}.Encode(), "test")
	if w.Code != 500 {
		t.Fatalf("failed save status %d", w.Code)
	}
	var n int
	a.db.QueryRow("SELECT COUNT(*) FROM foods").Scan(&n)
	if n != 0 {
		t.Fatal("failed entry save left a food behind")
	}
	execSQL(t, a.db, "DROP TRIGGER fail_entry")
	doForm(t, a.routes(), "POST", "/nutrition/entries", url.Values{"csrf": {"csrf"}, "draft_id": {token}}, true, 303)
}

func TestAIGatewayDoesNotFollowRedirectsAndUsesDeadline(t *testing.T) {
	a := testApp(t)
	mockGateway(t, a, sampleNutrition)
	calls := 0
	a.aiTransport = roundTripFunc(func(r *http.Request) (*http.Response, error) {
		calls++
		deadline, ok := r.Context().Deadline()
		if !ok || time.Until(deadline) > 60*time.Second || time.Until(deadline) < 55*time.Second {
			t.Error("analysis has no 60 second deadline")
		}
		resp := gatewayResponse(307, "")
		resp.Header.Set("Location", "https://other.test/collect")
		return resp, nil
	})
	if _, err := a.analyzeNutrition(context.Background(), foodInput{Description: "Oats", Grams: 100}); err == nil || calls != 1 {
		t.Fatal("gateway redirect was followed")
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	calls = 0
	a.aiTransport = roundTripFunc(func(r *http.Request) (*http.Response, error) { return nil, r.Context().Err() })
	if _, err := a.analyzeNutrition(ctx, foodInput{Description: "Oats", Grams: 100}); err == nil {
		t.Fatal("canceled request succeeded")
	}
}

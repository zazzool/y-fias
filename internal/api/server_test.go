package api

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/zazzool/y-fias/internal/geo"
	"github.com/zazzool/y-fias/internal/index"
)

func testServer(t *testing.T) http.Handler {
	t.Helper()

	snap := index.Build([]index.Street{
		{Name: "Саляма Адиля", Type: "улица", Region: "77", District: "Хорошёво-Мнёвники", Calls: 2751},
		{Name: "Тверская", Type: "улица", Region: "77", District: "Тверской", Calls: 120},
		{Name: "Тверской", Type: "бульвар", Region: "77", District: "Тверской", Calls: 40},
		{Name: "Вишнёвая", Type: "улица", Region: "50", District: "Раменский", Settlement: "СНТ Лесная поляна"},
	}, 20260818)

	snap.SetHouses(map[uint32][]string{
		0: {"2", "2к1", "7к7"},
	})

	areas := geo.NewIndex([]geo.Area{{
		Name:   "Тестовый",
		Region: "77",
		Polygons: []geo.Polygon{{
			Outer: geo.Ring{
				{Lon: 37, Lat: 55}, {Lon: 38, Lat: 55}, {Lon: 38, Lat: 56},
				{Lon: 37, Lat: 56}, {Lon: 37, Lat: 55},
			},
		}},
	}})

	return New(Options{Snapshots: index.NewHolder(snap), Areas: areas}).Handler()
}

func post(t *testing.T, h http.Handler, path string, body any) *httptest.ResponseRecorder {
	t.Helper()

	var buf bytes.Buffer
	if err := json.NewEncoder(&buf).Encode(body); err != nil {
		t.Fatal(err)
	}
	req := httptest.NewRequest(http.MethodPost, path, &buf)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	return rec
}

func TestSuggest(t *testing.T) {
	h := testServer(t)

	rec := post(t, h, "/v1/suggest", SuggestRequest{Query: "саляма", Limit: 5})
	if rec.Code != http.StatusOK {
		t.Fatalf("код %d: %s", rec.Code, rec.Body.String())
	}

	var resp SuggestResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatal(err)
	}
	if len(resp.Items) == 0 {
		t.Fatal("подсказок нет")
	}

	first := resp.Items[0]
	if first.Street != "Саляма Адиля" {
		t.Errorf("первая подсказка %q", first.Street)
	}
	// Название и контекст отдаются раздельно: склейку делает потребитель.
	if first.Type != "улица" || first.Full != "Саляма Адиля улица" {
		t.Errorf("тип и полное название разошлись: %+v", first)
	}
	if first.Layer == "" || first.Key == "" {
		t.Errorf("не заполнены служебные поля: %+v", first)
	}
}

func TestSuggestEmptyQuery(t *testing.T) {
	h := testServer(t)
	if rec := post(t, h, "/v1/suggest", SuggestRequest{}); rec.Code != http.StatusBadRequest {
		t.Errorf("на пустой запрос код %d, ожидался 400", rec.Code)
	}
}

func TestParse(t *testing.T) {
	h := testServer(t)

	rec := post(t, h, "/v1/parse", ParseRequest{Address: "улица Саляма Адиля, д. 2 к 1"})
	if rec.Code != http.StatusOK {
		t.Fatalf("код %d: %s", rec.Code, rec.Body.String())
	}

	var resp ParseResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatal(err)
	}

	if resp.Street != "саляма адиля" {
		t.Errorf("улица разобрана как %q", resp.Street)
	}
	if resp.House == nil {
		t.Fatal("дом не разобран")
	}
	if resp.House.Canonical != "2к1" {
		t.Errorf("каноническая форма дома %q, ожидалась «2к1»", resp.House.Canonical)
	}
	// Инвариант 1: исходный ввод возвращается неизменным.
	if resp.Raw != "улица Саляма Адиля, д. 2 к 1" {
		t.Errorf("ввод изменён: %q", resp.Raw)
	}
}

// Ответ обязан нести исходный ввод дома, а не только каноническую форму:
// показывать пользователю следует то, что он ввёл (§8.2).
func TestParseKeepsRawHouse(t *testing.T) {
	h := testServer(t)

	rec := post(t, h, "/v1/parse", ParseRequest{Address: "Тверская улица", House: "7 к 7"})
	var resp ParseResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatal(err)
	}
	if resp.House == nil {
		t.Fatal("дом не разобран")
	}
	if resp.House.Raw != "7 к 7" {
		t.Errorf("исходный номер дома %q, ожидался «7 к 7»", resp.House.Raw)
	}
	if resp.House.Canonical != "7к7" {
		t.Errorf("каноническая форма %q", resp.House.Canonical)
	}
}

func TestValidate(t *testing.T) {
	h := testServer(t)

	cases := []struct {
		name   string
		req    ValidateRequest
		status string
	}{
		{"точное совпадение", ValidateRequest{Street: "Саляма Адиля улица"}, "ok"},
		{"опечатка", ValidateRequest{Street: "Саляма Адиla"}, "warning"},
		{"нет такой улицы", ValidateRequest{Street: "Несуществующая зюзюка"}, "unknown"},
	}

	for _, c := range cases {
		rec := post(t, h, "/v1/validate", c.req)
		if rec.Code != http.StatusOK {
			t.Fatalf("%s: код %d: %s", c.name, rec.Code, rec.Body.String())
		}
		var resp ValidateResponse
		if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
			t.Fatal(err)
		}
		if resp.Status != c.status {
			t.Errorf("%s: статус %q, ожидался %q (сообщения: %v)",
				c.name, resp.Status, c.status, resp.Messages)
		}
	}
}

// Латинская раскладка распознаётся, но НЕ подставляется молча: ответ содержит
// предложение, и решение принимает человек.
func TestValidateSuggestsLayoutFixWithoutApplying(t *testing.T) {
	h := testServer(t)

	rec := post(t, h, "/v1/validate", ValidateRequest{Street: "Ndthcrfz"}) // «Тверская»
	var resp ValidateResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatal(err)
	}

	var mentioned bool
	for _, m := range resp.Messages {
		if strings.Contains(m, "раскладк") {
			mentioned = true
		}
	}
	if !mentioned {
		t.Errorf("о латинской раскладке не сказано: %v", resp.Messages)
	}
	if len(resp.Suggestions) == 0 {
		t.Error("исправление не предложено")
	}
}

// «Дома нет в справочнике» — не ошибка: справочник ФНС неполон, а вызов
// туда всё равно приедет.
func TestValidateHouseNotInGarIsNotAnError(t *testing.T) {
	h := testServer(t)

	rec := post(t, h, "/v1/validate", ValidateRequest{
		Street: "Саляма Адиля улица",
		House:  "999",
	})
	var resp ValidateResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatal(err)
	}

	if resp.Status != "ok" {
		t.Errorf("статус %q: улица найдена точно, отсутствие дома статус не портит", resp.Status)
	}
	if resp.Quality.House != "not_in_gar" {
		t.Errorf("качество дома %q, ожидалось not_in_gar", resp.Quality.House)
	}
}

func TestGeo(t *testing.T) {
	h := testServer(t)

	rec := post(t, h, "/v1/geo", GeoRequest{Lon: 37.5, Lat: 55.5})
	if rec.Code != http.StatusOK {
		t.Fatalf("код %d: %s", rec.Code, rec.Body.String())
	}
	var resp GeoResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatal(err)
	}
	if resp.District != "Тестовый" || resp.Confidence != "high" {
		t.Errorf("получено %+v", resp)
	}

	// Точка вне всех областей: ответ должен быть честным, а не пустым.
	rec = post(t, h, "/v1/geo", GeoRequest{Lon: 100, Lat: 10})
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatal(err)
	}
	if resp.Confidence != "none" {
		t.Errorf("вне областей уверенность %q, ожидалась none", resp.Confidence)
	}
}

func TestHealthAndReady(t *testing.T) {
	h := testServer(t)

	for _, path := range []string{"/healthz", "/readyz", "/metrics"} {
		req := httptest.NewRequest(http.MethodGet, path, nil)
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, req)
		if rec.Code != http.StatusOK {
			t.Errorf("%s: код %d", path, rec.Code)
		}
	}

	req := httptest.NewRequest(http.MethodGet, "/healthz", nil)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)

	var resp HealthResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatal(err)
	}
	if resp.SnapshotVer != 20260818 || resp.Streets != 4 {
		t.Errorf("состояние справочника не отражено: %+v", resp)
	}
	if resp.Houses != 3 {
		t.Errorf("домов %d, ожидалось 3", resp.Houses)
	}
}

// Пустой справочник означает «не готов»: балансировщик не должен слать
// запросы на процесс, который ответит пустотой и будет выглядеть исправным.
func TestReadyFailsWithoutSnapshot(t *testing.T) {
	h := New(Options{Snapshots: index.NewHolder(index.Build(nil, 0))}).Handler()

	req := httptest.NewRequest(http.MethodGet, "/readyz", nil)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)

	if rec.Code != http.StatusServiceUnavailable {
		t.Errorf("код %d, ожидался 503", rec.Code)
	}
}

func TestRejectsMalformedJSON(t *testing.T) {
	h := testServer(t)

	req := httptest.NewRequest(http.MethodPost, "/v1/suggest",
		strings.NewReader(`{"query": "тверская", "неизвестное": 1}`))
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)

	// Неизвестные поля отвергаются: молча проглоченная опечатка в имени
	// поля означала бы, что клиент думает, будто передал параметр.
	if rec.Code != http.StatusBadRequest {
		t.Errorf("код %d, ожидался 400", rec.Code)
	}
}

func BenchmarkSuggestHTTP(b *testing.B) {
	snap := index.Build([]index.Street{
		{Name: "Саляма Адиля", Type: "улица", Region: "77"},
		{Name: "Тверская", Type: "улица", Region: "77"},
	}, 1)
	h := New(Options{Snapshots: index.NewHolder(snap)}).Handler()

	body := []byte(`{"query":"саляма"}`)
	b.ReportAllocs()
	b.ResetTimer()
	for b.Loop() {
		req := httptest.NewRequest(http.MethodPost, "/v1/suggest", bytes.NewReader(body))
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, req)
	}
}

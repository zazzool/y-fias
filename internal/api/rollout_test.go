package api

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/zazzool/y-fias/internal/index"
	"github.com/zazzool/y-fias/internal/rollout"
)

func rolloutServer(t *testing.T, defaults map[string]int) (http.Handler, *Metrics) {
	t.Helper()
	snap := index.Build([]index.Street{{Name: "Тверская", Type: "улица", Region: "77"}}, 1)
	m := NewMetrics()
	h := New(Options{
		Snapshots: index.NewHolder(snap),
		Metrics:   m,
		Rollout:   rollout.New(defaults, nil),
	}).Handler()
	return h, m
}

func put(t *testing.T, h http.Handler, path, body string) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(http.MethodPut, path, strings.NewReader(body))
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	return rec
}

func getRollout(t *testing.T, h http.Handler) RolloutResponse {
	t.Helper()
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/v1/rollout", nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("GET /v1/rollout: %d %s", rec.Code, rec.Body.String())
	}
	var resp RolloutResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatal(err)
	}
	return resp
}

func TestRolloutDefaultsOff(t *testing.T) {
	h, _ := rolloutServer(t, nil)
	resp := getRollout(t, h)

	for _, e := range rollout.Endpoints {
		p, ok := resp.Flags[e]
		if !ok || p != 0 {
			t.Errorf("%s: %d%% (есть=%v), без решения должен быть 0", e, p, ok)
		}
	}
	if resp.Hash != rollout.HashName || resp.TTLSeconds <= 0 {
		t.Errorf("клиенту не сказано, как делить и сколько кэшировать: %+v", resp)
	}
}

func TestRolloutRaiseAndRollback(t *testing.T) {
	h, m := rolloutServer(t, nil)

	rec := put(t, h, "/v1/rollout", `{"endpoint":"suggest","percent":10,"reason":"старт","actor":"admin"}`)
	if rec.Code != http.StatusOK {
		t.Fatalf("повышение: %d %s", rec.Code, rec.Body.String())
	}
	var set RolloutSetResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &set); err != nil {
		t.Fatal(err)
	}
	if set.Flags["suggest"] != 10 || set.Change.From != 0 || set.Change.To != 10 {
		t.Errorf("повышение: %+v", set)
	}
	// Без базы изменение действует, но администратор обязан узнать, что
	// оно не переживёт перезапуск.
	if set.Persisted || set.Warning == "" {
		t.Errorf("нет предупреждения о несохранённом изменении: %+v", set)
	}

	// Откат — без причины, мгновенно.
	rec = put(t, h, "/v1/rollout", `{"endpoint":"suggest","percent":0}`)
	if rec.Code != http.StatusOK {
		t.Fatalf("откат: %d %s", rec.Code, rec.Body.String())
	}
	if p := getRollout(t, h).Flags["suggest"]; p != 0 {
		t.Errorf("после отката suggest = %d%%", p)
	}

	metrics := httptest.NewRecorder()
	m.Handler().ServeHTTP(metrics, httptest.NewRequest(http.MethodGet, "/metrics", nil))
	body := metrics.Body.String()
	for _, want := range []string{
		`yfias_rollout_percent{endpoint="suggest"} 0`,
		`yfias_rollout_rollbacks_total{endpoint="suggest"} 1`,
	} {
		if !strings.Contains(body, want) {
			t.Errorf("в метриках нет %q", want)
		}
	}
}

func TestRolloutRejects(t *testing.T) {
	h, _ := rolloutServer(t, map[string]int{"validate": 50})

	cases := map[string]string{
		"повышение без причины": `{"endpoint":"validate","percent":100}`,
		"запись долями нельзя":  `{"endpoint":"resolve","percent":10,"reason":"x"}`,
		"больше 100":            `{"endpoint":"suggest","percent":150,"reason":"x"}`,
		// Пропущенная доля не должна читаться как 0, то есть как откат.
		"без доли": `{"endpoint":"validate"}`,
	}
	for name, body := range cases {
		if rec := put(t, h, "/v1/rollout", body); rec.Code != http.StatusBadRequest {
			t.Errorf("%s: код %d, ожидался 400", name, rec.Code)
		}
	}
	if p := getRollout(t, h).Flags["validate"]; p != 50 {
		t.Errorf("отклонённые запросы изменили долю: %d%%", p)
	}
}

func TestRolloutHistoryNeedsStore(t *testing.T) {
	h, _ := rolloutServer(t, nil)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/v1/rollout/history", nil))
	if rec.Code != http.StatusServiceUnavailable {
		t.Errorf("без журнала история должна честно отвечать 503, получено %d", rec.Code)
	}
}

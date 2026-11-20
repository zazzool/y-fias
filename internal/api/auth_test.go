package api

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/zazzool/y-fias/internal/auth"
	"github.com/zazzool/y-fias/internal/index"
	"github.com/zazzool/y-fias/internal/rollout"
)

type authFixture struct {
	h                     http.Handler
	m                     *Metrics
	client, admin, metric string
}

func newAuthFixture(t *testing.T) authFixture {
	t.Helper()
	gen := func(name string, role auth.Role) (string, string) {
		k, l, err := auth.Generate(name, role)
		if err != nil {
			t.Fatal(err)
		}
		return k, l
	}
	ck, cl := gen("mis-backend", auth.RoleClient)
	ak, al := gen("mis-admin", auth.RoleAdmin)
	mk, ml := gen("prometheus", auth.RoleMetrics)
	kr, err := auth.Parse(strings.Join([]string{cl, al, ml}, "\n"))
	if err != nil {
		t.Fatal(err)
	}

	snap := index.Build([]index.Street{{Name: "Тверская", Type: "улица", Region: "77"}}, 1)
	m := NewMetrics()
	h := New(Options{
		Snapshots: index.NewHolder(snap),
		Metrics:   m,
		Rollout:   rollout.New(nil, nil),
		Auth:      kr,
	}).Handler()
	return authFixture{h: h, m: m, client: ck, admin: ak, metric: mk}
}

func (f authFixture) do(method, path, key, body string) *httptest.ResponseRecorder {
	req := httptest.NewRequest(method, path, strings.NewReader(body))
	if key != "" {
		req.Header.Set("X-API-Key", key)
	}
	rec := httptest.NewRecorder()
	f.h.ServeHTTP(rec, req)
	return rec
}

// Матрица доступа: кто что может. Это и есть спецификация ролей.
func TestAuthMatrix(t *testing.T) {
	f := newAuthFixture(t)

	type call struct{ method, path, body string }
	suggest := call{"POST", "/v1/suggest", `{"query":"тверская"}`}
	flags := call{"GET", "/v1/rollout", ""}
	setFlag := call{"PUT", "/v1/rollout", `{"endpoint":"suggest","percent":0}`}
	report := call{"GET", "/v1/shadow/cases", ""}
	metrics := call{"GET", "/metrics", ""}

	cases := []struct {
		name string
		c    call
		key  string
		want int
	}{
		{"подсказка без ключа", suggest, "", 401},
		{"подсказка с чужим ключом", suggest, "yf_неизвестный", 401},
		{"подсказка, client", suggest, f.client, 200},
		{"подсказка, admin", suggest, f.admin, 200},
		{"подсказка, metrics", suggest, f.metric, 403},

		{"флаги, client", flags, f.client, 200},
		{"смена флага, client", setFlag, f.client, 403},
		{"смена флага, admin", setFlag, f.admin, 200},

		// Отчёт отвечает 503 без базы — главное, что не 401/403.
		{"отчёт, client", report, f.client, 403},
		{"отчёт, admin", report, f.admin, 503},

		{"метрики без ключа", metrics, "", 401},
		{"метрики, client", metrics, f.client, 403},
		{"метрики, metrics", metrics, f.metric, 200},
		{"метрики, admin", metrics, f.admin, 200},
	}
	for _, c := range cases {
		rec := f.do(c.c.method, c.c.path, c.key, c.c.body)
		if rec.Code != c.want {
			t.Errorf("%s: код %d, ожидался %d (%s)", c.name, rec.Code, c.want, rec.Body.String())
		}
		if rec.Code == 401 && rec.Header().Get("WWW-Authenticate") == "" {
			t.Errorf("%s: 401 без WWW-Authenticate", c.name)
		}
	}
}

// Проверки живости открыты: их вызывает healthcheck Docker без ключа.
func TestHealthOpen(t *testing.T) {
	f := newAuthFixture(t)
	for _, p := range []string{"/healthz", "/readyz"} {
		if rec := f.do("GET", p, "", ""); rec.Code != 200 {
			t.Errorf("%s без ключа: %d", p, rec.Code)
		}
	}
}

func TestAuthBearer(t *testing.T) {
	f := newAuthFixture(t)
	req := httptest.NewRequest("GET", "/v1/rollout", nil)
	req.Header.Set("Authorization", "Bearer "+f.client)
	rec := httptest.NewRecorder()
	f.h.ServeHTTP(rec, req)
	if rec.Code != 200 {
		t.Errorf("Bearer: %d", rec.Code)
	}
}

// Ключ в строке запроса не принимается: URL оседает в журналах прокси.
func TestAuthNotFromQuery(t *testing.T) {
	f := newAuthFixture(t)
	if rec := f.do("GET", "/v1/rollout?api_key="+f.client, "", ""); rec.Code != 401 {
		t.Errorf("ключ из URL принят: %d", rec.Code)
	}
}

// Изменение флага без указанного человека подписывается именем ключа.
func TestRolloutActorFromKey(t *testing.T) {
	f := newAuthFixture(t)
	rec := f.do("PUT", "/v1/rollout", f.admin, `{"endpoint":"geo","percent":10,"reason":"тест"}`)
	if rec.Code != 200 {
		t.Fatalf("%d %s", rec.Code, rec.Body.String())
	}
	var resp RolloutSetResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatal(err)
	}
	if resp.Change.Actor != "key:mis-admin" {
		t.Errorf("автор изменения %q", resp.Change.Actor)
	}

	rec = f.do("PUT", "/v1/rollout", f.admin, `{"endpoint":"geo","percent":0,"actor":"ivanov"}`)
	_ = json.Unmarshal(rec.Body.Bytes(), &resp)
	if resp.Change.Actor != "ivanov (key:mis-admin)" {
		t.Errorf("автор с ключом %q", resp.Change.Actor)
	}
}

func TestAuthFailureMetrics(t *testing.T) {
	f := newAuthFixture(t)
	f.do("POST", "/v1/suggest", "", `{}`)
	f.do("POST", "/v1/suggest", "yf_x", `{}`)
	f.do("PUT", "/v1/rollout", f.client, `{}`)

	rec := f.do("GET", "/metrics", f.metric, "")
	for _, want := range []string{
		`yfias_auth_failures_total{reason="missing"} 1`,
		`yfias_auth_failures_total{reason="invalid"} 1`,
		`yfias_auth_failures_total{reason="forbidden"} 1`,
	} {
		if !strings.Contains(rec.Body.String(), want) {
			t.Errorf("в метриках нет %q", want)
		}
	}
}

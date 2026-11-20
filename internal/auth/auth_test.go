package auth

import (
	"net/http/httptest"
	"strings"
	"testing"
)

func TestGenerateAndLookup(t *testing.T) {
	key, line, err := Generate("mis-backend", RoleClient)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(key, KeyPrefix) || len(key) < 40 {
		t.Fatalf("ключ %q", key)
	}
	if strings.Contains(line, key) {
		t.Fatal("строка для сервера содержит сам ключ")
	}

	kr, err := Parse("# ключи\n\n" + line + "\n")
	if err != nil {
		t.Fatal(err)
	}
	k, ok := kr.Lookup(key)
	if !ok || k.Name != "mis-backend" || k.Role != RoleClient {
		t.Fatalf("ключ не опознан: %+v %v", k, ok)
	}
	if _, ok := kr.Lookup(key + "x"); ok {
		t.Error("опознан изменённый ключ")
	}
	if _, ok := kr.Lookup(""); ok {
		t.Error("опознан пустой ключ")
	}
}

func TestKeysAreUnique(t *testing.T) {
	a, _, _ := Generate("a", RoleClient)
	b, _, _ := Generate("a", RoleClient)
	if a == b {
		t.Fatal("два ключа совпали")
	}
}

func TestParseCommaSeparated(t *testing.T) {
	_, l1, _ := Generate("mis", RoleClient)
	_, l2, _ := Generate("admin", RoleAdmin)
	kr, err := Parse(l1 + "," + l2)
	if err != nil {
		t.Fatal(err)
	}
	if kr.Len() != 2 {
		t.Fatalf("ключей %d", kr.Len())
	}
}

func TestParseRejects(t *testing.T) {
	key, line, _ := Generate("mis", RoleClient)
	good := strings.Fields(line)[2]
	cases := map[string]string{
		"пусто":            "# только комментарий\n",
		"ключ вместо хеша": "mis client " + key,
		"без sha256:":      "mis client " + strings.TrimPrefix(good, "sha256:"),
		"короткий хеш":     "mis client sha256:abcd",
		"роль":             "mis root " + good,
		"имя кириллицей":   "мис client " + good,
		"повтор имени":     line + "\n" + line,
		"не хватает поля":  "mis " + good,
	}
	for name, text := range cases {
		if _, err := Parse(text); err == nil {
			t.Errorf("%s: принято", name)
		}
	}
}

func TestRoles(t *testing.T) {
	if !RoleAdmin.Allows(RoleClient) || !RoleAdmin.Allows(RoleMetrics) {
		t.Error("admin должен покрывать client и metrics")
	}
	if RoleClient.Allows(RoleAdmin) || RoleMetrics.Allows(RoleClient) || RoleClient.Allows(RoleMetrics) {
		t.Error("роль шире положенного")
	}
}

func TestFromRequest(t *testing.T) {
	r := httptest.NewRequest("GET", "/?api_key=yf_query", nil)
	if k := FromRequest(r); k != "" {
		t.Errorf("ключ из строки запроса принят: %q", k)
	}
	r.Header.Set("Authorization", "Bearer yf_bearer")
	if k := FromRequest(r); k != "yf_bearer" {
		t.Errorf("Bearer: %q", k)
	}
	r.Header.Set("X-API-Key", "yf_header")
	if k := FromRequest(r); k != "yf_header" {
		t.Errorf("X-API-Key важнее: %q", k)
	}
}

package rollout

import (
	"context"
	"errors"
	"fmt"
	"testing"
)

// Контрольные значения корзин. Те же числа стоят в клиенте МИС
// (docs/07-rollout.md): расхождение означает, что Go и TypeScript делят
// диспетчеров по-разному, и доля на графиках не совпадёт с долей на деле.
func TestBucketVectors(t *testing.T) {
	cases := []struct {
		endpoint, key string
		want          int
	}{
		{"suggest", "", 77},
		{"suggest", "disp-1", 63},
		{"suggest", "disp-2", 82},
		{"validate", "disp-1", 45},
		{"geo", "42", 82},
		{"parse", "иванова", 67},
	}
	for _, c := range cases {
		if got := Bucket(c.endpoint, c.key); got != c.want {
			t.Errorf("Bucket(%q, %q) = %d, ожидалось %d", c.endpoint, c.key, got, c.want)
		}
	}
}

// Рост доли не выбрасывает уже включённых: кто получал Y-FIAS при 10%,
// получает его и при 50%. Иначе раскатка шла бы через перетасовку.
func TestServesMonotonic(t *testing.T) {
	for i := range 2000 {
		key := fmt.Sprintf("d%d", i)
		was := false
		for p := 0; p <= 100; p++ {
			now := Serves(p, Suggest, key)
			if was && !now {
				t.Fatalf("ключ %s выпал при росте доли до %d%%", key, p)
			}
			was = now
		}
	}
}

func TestServesShare(t *testing.T) {
	const n = 20000
	for _, p := range []int{10, 50} {
		in := 0
		for i := range n {
			if Serves(p, Suggest, fmt.Sprintf("k%d", i)) {
				in++
			}
		}
		got := float64(in) * 100 / n
		if got < float64(p)-2 || got > float64(p)+2 {
			t.Errorf("доля %d%%: включено %.1f%%", p, got)
		}
	}
}

func TestServesWithoutKey(t *testing.T) {
	if Serves(50, Suggest, "") {
		t.Error("без ключа при неполной доле запрос должен идти в прежнюю систему")
	}
	if !Serves(100, Suggest, "") {
		t.Error("при полной доле ключ не нужен")
	}
}

// Методы делят диспетчеров независимо.
func TestEndpointsIndependent(t *testing.T) {
	same := 0
	for i := range 1000 {
		k := fmt.Sprintf("k%d", i)
		if Serves(10, Suggest, k) == Serves(10, Validate, k) {
			same++
		}
	}
	if same == 1000 {
		t.Error("suggest и validate включаются у одних и тех же диспетчеров")
	}
}

type fakeStore struct {
	saved   map[string]int
	changes []Change
	failing bool
}

func (f *fakeStore) LoadRollout(context.Context) (map[string]int, error) {
	if f.failing {
		return nil, errors.New("база недоступна")
	}
	return f.saved, nil
}

func (f *fakeStore) SaveRollout(_ context.Context, c Change) error {
	if f.failing {
		return errors.New("база недоступна")
	}
	if f.saved == nil {
		f.saved = map[string]int{}
	}
	f.saved[c.Endpoint] = c.To
	f.changes = append(f.changes, c)
	return nil
}

func TestDefaultsOff(t *testing.T) {
	r := New(nil, nil)
	for _, e := range Endpoints {
		if p := r.Percent(e); p != 0 {
			t.Errorf("%s = %d%% без явного решения", e, p)
		}
	}
}

// Сохранённое значение важнее флага запуска: ручной откат не должен
// отменяться перезапуском с прежней командной строкой.
func TestSavedOverridesDefaults(t *testing.T) {
	st := &fakeStore{saved: map[string]int{Suggest: 0, Validate: 50}}
	r := New(map[string]int{Suggest: 50}, st)
	if err := r.Load(context.Background()); err != nil {
		t.Fatal(err)
	}
	if p := r.Percent(Suggest); p != 0 {
		t.Errorf("suggest = %d%%, ожидался сохранённый откат до 0", p)
	}
	if p := r.Percent(Validate); p != 50 {
		t.Errorf("validate = %d%%", p)
	}
}

// Откат работает и при лежащей базе — в этот момент он нужнее всего.
func TestRollbackWithoutDatabase(t *testing.T) {
	st := &fakeStore{failing: true}
	r := New(map[string]int{Suggest: 50}, st)

	res, err := r.Set(context.Background(), Suggest, 0, "", "")
	if err != nil {
		t.Fatalf("откат отклонён: %v", err)
	}
	if r.Percent(Suggest) != 0 {
		t.Error("откат не применился в памяти")
	}
	if res.Persisted || res.SaveErr == nil {
		t.Error("ответ должен честно сказать, что откат не сохранён")
	}
	if !res.Change.Rollback() {
		t.Error("снижение доли не опознано как откат")
	}
}

func TestIncreaseNeedsReason(t *testing.T) {
	r := New(nil, &fakeStore{})
	_, err := r.Set(context.Background(), Suggest, 10, "  ", "")
	if !errors.Is(err, ErrReasonRequired) {
		t.Fatalf("ожидался отказ без причины, получено %v", err)
	}
	if r.Percent(Suggest) != 0 {
		t.Error("отклонённое изменение применилось")
	}
}

func TestSetPersists(t *testing.T) {
	st := &fakeStore{}
	r := New(nil, st)
	res, err := r.Set(context.Background(), Validate, 10, "первая доля", "admin")
	if err != nil {
		t.Fatal(err)
	}
	if !res.Persisted {
		t.Fatalf("не сохранено: %v", res.SaveErr)
	}
	if len(st.changes) != 1 || st.changes[0].From != 0 || st.changes[0].To != 10 ||
		st.changes[0].Actor != "admin" {
		t.Errorf("журнал изменений: %+v", st.changes)
	}
	if r.State().Updated.IsZero() {
		t.Error("время изменения не выставлено")
	}
}

func TestSetValidation(t *testing.T) {
	r := New(nil, nil)
	if _, err := r.Set(context.Background(), "resolve", 10, "x", ""); !errors.Is(err, ErrUnknownEndpoint) {
		t.Errorf("запись переключать долями нельзя, получено %v", err)
	}
	if _, err := r.Set(context.Background(), Suggest, 101, "x", ""); !errors.Is(err, ErrPercentRange) {
		t.Errorf("101%%: %v", err)
	}
}

func TestParseSpec(t *testing.T) {
	got, err := ParseSpec(" suggest=10, validate=100% ,")
	if err != nil {
		t.Fatal(err)
	}
	if got[Suggest] != 10 || got[Validate] != 100 || len(got) != 2 {
		t.Errorf("разбор: %v", got)
	}
	for _, bad := range []string{"suggest", "resolve=10", "geo=abc", "parse=150"} {
		if _, err := ParseSpec(bad); err == nil {
			t.Errorf("%q принят", bad)
		}
	}
}

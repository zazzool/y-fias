package api

import (
	"net/http/httptest"
	"testing"
	"time"

	"github.com/zazzool/y-fias/internal/store"
)

func shadowFilterFromURL(t *testing.T, url string) (store.ShadowFilter, int) {
	t.Helper()
	return shadowFilter(httptest.NewRequest("GET", url, nil))
}

// TestGateOf проверяет формулировку условия выхода этапа 3.
//
// Условие содержательное, а не арифметическое: «ноль worse» на пустой выборке
// не означает готовность, и страница обязана отличать одно от другого.
func TestGateOf(t *testing.T) {
	big := store.VerdictCounts{Total: 5000, Same: 4800, Better: 200}
	judged := store.VerdictCounts{Total: 1200, Same: 1000, Better: 200}

	tests := []struct {
		name      string
		rep       store.ShadowReport
		wantReady bool
		wantWhy   string
	}{
		{
			name:      "мало данных",
			rep:       store.ShadowReport{Totals: store.VerdictCounts{Total: 12}, Judged: store.VerdictCounts{Total: 12}},
			wantReady: false,
			wantWhy:   "данных мало: ноль расхождений на такой выборке ничего не доказывает",
		},
		{
			name: "есть неразобранные worse",
			rep: store.ShadowReport{
				Totals: big, Judged: judged,
				Review:    store.ReviewStats{WorseTotal: 3, Open: 2},
				LastWorse: time.Now().Add(-2 * time.Hour),
			},
			wantReady: false,
			wantWhy:   "есть неразобранные расхождения класса «прежняя ответила лучше»",
		},
		{
			name: "выборка есть, выбора человека нет",
			rep: store.ShadowReport{
				Totals: big,
				Judged: store.VerdictCounts{},
			},
			wantReady: false,
			wantWhy:   "ни одного сравнения с выбором человека: судить не о чем",
		},
		{
			name: "worse были, но все разобраны",
			rep: store.ShadowReport{
				Totals: big, Judged: judged,
				Review:    store.ReviewStats{WorseTotal: 4, Open: 0, Fixed: 3, FalseAlarm: 1},
				LastWorse: time.Now().Add(-72 * time.Hour),
			},
			wantReady: true,
			wantWhy:   "блокирующих расхождений нет, все разобраны",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			g := gateOf(&tt.rep)
			if g.Ready != tt.wantReady {
				t.Errorf("Ready = %v, ожидалось %v (причина: %s)", g.Ready, tt.wantReady, g.Reason)
			}
			if g.Reason != tt.wantWhy {
				t.Errorf("Reason = %q, ожидалось %q", g.Reason, tt.wantWhy)
			}
		})
	}
}

// TestGateCleanHours: время с последнего блокирующего расхождения показывается
// только если оно вообще было. Иначе «0 часов чисто» читалось бы как «только
// что сломалось».
func TestGateCleanHours(t *testing.T) {
	never := gateOf(&store.ShadowReport{Totals: store.VerdictCounts{Total: 900}, Judged: store.VerdictCounts{Total: 900}})
	if never.HadWorse || never.CleanHours != 0 {
		t.Errorf("без worse ожидались HadWorse=false и CleanHours=0, получено %+v", never)
	}

	had := gateOf(&store.ShadowReport{
		Totals: store.VerdictCounts{Total: 900}, Judged: store.VerdictCounts{Total: 900},
		LastWorse: time.Now().Add(-5 * time.Hour),
	})
	if !had.HadWorse || had.CleanHours < 4.9 || had.CleanHours > 5.1 {
		t.Errorf("ожидалось ~5 часов с последнего worse, получено %+v", had)
	}
}

// TestShadowFilterIgnoresJunk: устаревшая закладка не должна ломать отчёт.
func TestShadowFilterIgnoresJunk(t *testing.T) {
	f, days := shadowFilterFromURL(t, "/v1/shadow/report?days=нет&method=магия&verdict=&review=потом&q=тверская")

	if days != 7 {
		t.Errorf("days = %d, ожидалось значение по умолчанию 7", days)
	}
	if f.Method != "" || f.Verdict != "" || f.Review != "" {
		t.Errorf("непонятные срезы должны игнорироваться, получено %+v", f)
	}
	if f.Query != "тверская" {
		t.Errorf("Query = %q, ожидалось «тверская»", f.Query)
	}
}

// TestShadowFilterClampsWindow: окно ограничено, иначе один запрос способен
// поднять из журнала всё, что там накопилось.
func TestShadowFilterClampsWindow(t *testing.T) {
	_, days := shadowFilterFromURL(t, "/v1/shadow/report?days=100000")
	if days != 90 {
		t.Errorf("days = %d, ожидалось ограничение 90", days)
	}

	f, days := shadowFilterFromURL(t, "/v1/shadow/report?days=1")
	if days != 1 {
		t.Errorf("days = %d, ожидалось 1", days)
	}
	if time.Since(f.Since) > 25*time.Hour {
		t.Errorf("окно шире суток: %v", f.Since)
	}
}

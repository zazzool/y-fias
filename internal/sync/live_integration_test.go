//go:build integration

package sync

import (
	"context"
	"net/http"
	"sort"
	"testing"
	"time"

	"github.com/zazzool/y-fias/internal/gar"
)

// TestLive_PlanAgainstRealVersions проверяет планирование на настоящем списке
// публикаций ФНС.
//
// Модульные тесты работают на выдуманных версиях и не поймают, если источник
// изменит форму ответа: перестанет отдавать дельты, начнёт нумеровать иначе,
// сократит глубину истории. Здесь проверяется именно это.
func TestLive_PlanAgainstRealVersions(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()

	client := &http.Client{Timeout: time.Minute}
	versions, err := gar.AllVersions(ctx, client)
	if err != nil {
		t.Skipf("источник недоступен: %v", err)
	}

	if len(versions) < 10 {
		t.Fatalf("версий всего %d — источник изменил поведение", len(versions))
	}

	sorted := append([]gar.VersionInfo(nil), versions...)
	sort.Slice(sorted, func(i, j int) bool { return sorted[i].VersionID < sorted[j].VersionID })
	oldest, latest := sorted[0], sorted[len(sorted)-1]

	t.Logf("версий доступно %d, от %d до %d", len(versions), oldest.VersionID, latest.VersionID)

	// Глубина истории — основание для решения «дельты или пересборка».
	// Если ФНС резко сократит её, полная пересборка станет требоваться чаще,
	// и об этом надо узнать из теста, а не из аварии.
	var withDelta int
	for _, v := range versions {
		if v.DeltaURL != "" {
			withDelta++
		}
	}
	t.Logf("с дельтой: %d из %d", withDelta, len(versions))
	if withDelta < len(versions)/2 {
		t.Errorf("дельты есть лишь у %d из %d версий — дельта-синхронизация "+
			"перестала быть основным путём", withDelta, len(versions))
	}

	cases := []struct {
		name    string
		current int
		want    Mode
	}{
		{"мы на последней", latest.VersionID, ModeUpToDate},
		{"мы на предпоследней", sorted[len(sorted)-2].VersionID, ModeDelta},
		{"мы на самой старой доступной", oldest.VersionID, ModeDelta},
		{"мы старше всей истории", oldest.VersionID - 1000, ModeFull},
		{"справочник не собирался", 0, ModeFull},
	}

	for _, c := range cases {
		p, err := Build(c.current, versions)
		if err != nil {
			t.Fatalf("%s: %v", c.name, err)
		}
		if p.Mode != c.want {
			t.Errorf("%s: режим %v, ожидался %v (%s)", c.name, p.Mode, c.want, p.Reason)
			continue
		}
		t.Logf("%-30s -> %-18s дельт %d", c.name, p.Mode, p.LagVersions())
	}

	// Догоняя с самой старой версии, мы должны применить всю историю.
	p, _ := Build(oldest.VersionID, versions)
	if p.LagVersions() != len(versions)-1 {
		t.Errorf("с самой старой версии дельт %d, а версий всего %d",
			p.LagVersions(), len(versions))
	}
}

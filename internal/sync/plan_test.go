package sync

import (
	"testing"

	"github.com/zazzool/y-fias/internal/gar"
)

func versions(ids ...int) []gar.VersionInfo {
	out := make([]gar.VersionInfo, 0, len(ids))
	for _, id := range ids {
		out = append(out, gar.VersionInfo{
			VersionID: id,
			FullURL:   "https://example/full",
			DeltaURL:  "https://example/delta",
		})
	}
	return out
}

func TestBuildPlan(t *testing.T) {
	// Порядок в списке намеренно перемешан: источник отдаёт версии от новой
	// к старой, а применять их надо от старой к новой.
	avail := versions(20260814, 20260807, 20260811, 20260804)

	cases := []struct {
		name    string
		current int
		mode    Mode
		deltas  int
	}{
		{"актуально", 20260814, ModeUpToDate, 0},
		{"одна дельта позади", 20260811, ModeDelta, 1},
		{"три дельты позади", 20260804, ModeDelta, 3},
		{"справочник не собирался", 0, ModeFull, 0},
		{"версия старше доступных", 20260101, ModeFull, 0},
		{"версия новее доступных", 20270101, ModeUpToDate, 0},
	}

	for _, c := range cases {
		p, err := Build(c.current, avail)
		if err != nil {
			t.Fatalf("%s: %v", c.name, err)
		}
		if p.Mode != c.mode {
			t.Errorf("%s: режим %v, ожидался %v (%s)", c.name, p.Mode, c.mode, p.Reason)
		}
		if len(p.Deltas) != c.deltas {
			t.Errorf("%s: дельт %d, ожидалось %d", c.name, len(p.Deltas), c.deltas)
		}
		if p.Reason == "" {
			t.Errorf("%s: не указана причина выбора режима", c.name)
		}
	}
}

// Дельты обязаны идти строго по возрастанию: ГАР это журнал изменений, и
// применение их вразнобой даёт неверное состояние.
func TestDeltasAreOrdered(t *testing.T) {
	// Отправная версия обязана быть ИЗ списка доступных. Первая редакция
	// теста брала 20260801, которой в списке нет и которая старше самой
	// старой доступной, — и получала полную пересборку. Это верное
	// поведение: дельты за пропущенный промежуток уже не публикуются.
	p, err := Build(20260804, versions(20260814, 20260804, 20260811, 20260807))
	if err != nil {
		t.Fatal(err)
	}
	if p.Mode != ModeDelta {
		t.Fatalf("режим %v, ожидались дельты (%s)", p.Mode, p.Reason)
	}
	if len(p.Deltas) != 3 {
		t.Fatalf("дельт %d, ожидалось 3", len(p.Deltas))
	}
	for i := 1; i < len(p.Deltas); i++ {
		if p.Deltas[i-1].VersionID >= p.Deltas[i].VersionID {
			t.Fatalf("порядок нарушен: %d перед %d",
				p.Deltas[i-1].VersionID, p.Deltas[i].VersionID)
		}
	}
}

// Разрыв в цепочке дельт означает полную пересборку: пропустить публикацию
// нельзя.
func TestGapInDeltasForcesFullRebuild(t *testing.T) {
	avail := versions(20260804, 20260807, 20260811)
	avail[1].DeltaURL = "" // у средней версии дельты нет

	p, err := Build(20260801, avail)
	if err != nil {
		t.Fatal(err)
	}
	if p.Mode != ModeFull {
		t.Errorf("режим %v, ожидалась полная пересборка (%s)", p.Mode, p.Reason)
	}
	if len(p.Deltas) != 0 {
		t.Errorf("при полной пересборке дельты не нужны, их %d", len(p.Deltas))
	}
}

// Версия новее доступных — признак отката публикации у ФНС. Молча
// «обновляться назад» нельзя: справочник откатился бы незаметно.
func TestNewerThanSourceIsNotSilent(t *testing.T) {
	p, err := Build(20270101, versions(20260814))
	if err != nil {
		t.Fatal(err)
	}
	if p.Mode != ModeUpToDate {
		t.Errorf("режим %v: откатываться назад нельзя", p.Mode)
	}
	if p.Reason == "" || len(p.Reason) < 20 {
		t.Errorf("причина должна объяснять ситуацию человеку, получено %q", p.Reason)
	}
}

func TestEmptySourceIsError(t *testing.T) {
	if _, err := Build(1, nil); err == nil {
		t.Error("пустой список версий должен быть ошибкой")
	}
}

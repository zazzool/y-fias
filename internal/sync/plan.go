// Package sync поддерживает справочник в актуальном состоянии.
//
// Справочник, собранный один раз, расходится с реальностью с первого дня:
// улицы переименовывают, дома вводят и сносят, границы районов перекраивают.
// ФНС публикует изменения дважды в неделю, и дельта по нашим регионам стоит
// около 135 КБ против 53 ГБ полного архива.
//
// Главное здесь — НЕ обновление справочника, а сверка с нашими адресами.
// ГАР для нас справочник, а не начальство над записями: в addresses лежат
// адреса, куда уже выезжали бригады. Инвариант 1 действует и при
// синхронизации (§11.4 архитектуры).
package sync

import (
	"fmt"
	"sort"

	"github.com/zazzool/y-fias/internal/gar"
)

// Mode — как обновляться.
type Mode uint8

const (
	// ModeUpToDate — наша версия и есть последняя, делать нечего.
	ModeUpToDate Mode = iota
	// ModeDelta — применить дельты по порядку.
	ModeDelta
	// ModeFull — полная пересборка.
	ModeFull
)

func (m Mode) String() string {
	switch m {
	case ModeUpToDate:
		return "актуально"
	case ModeDelta:
		return "дельты"
	case ModeFull:
		return "полная пересборка"
	}
	return "?"
}

// Plan — что нужно сделать, чтобы догнать источник.
type Plan struct {
	Mode Mode

	// Current — наша версия, Target — куда идём.
	Current int
	Target  int

	// Deltas — версии к применению, СТРОГО по возрастанию.
	Deltas []gar.VersionInfo

	// FullURL — полный архив, если нужна пересборка.
	FullURL string

	// Reason — почему выбран такой режим. Попадает в журнал: разбирая
	// аварию, важно знать не только что сделали, но и почему.
	Reason string
}

// LagVersions — на сколько публикаций мы отстали.
func (p Plan) LagVersions() int { return len(p.Deltas) }

// Build решает, как догнать источник.
//
// Правила §11.3 архитектуры:
//   - дельты применяются строго по возрастанию версии, пропуск недопустим:
//     ГАР это журнал изменений, и порядок в нём значим;
//   - если наша версия старше самой старой доступной, дельты не помогут —
//     нужна полная пересборка. Это штатная ветка, а не ошибка: ФНС держит
//     онлайн около полугода.
func Build(current int, available []gar.VersionInfo) (Plan, error) {
	if len(available) == 0 {
		return Plan{}, fmt.Errorf("источник не вернул ни одной версии")
	}

	// Копия: сортировка не должна менять то, что нам передали.
	vs := append([]gar.VersionInfo(nil), available...)
	sort.Slice(vs, func(i, j int) bool { return vs[i].VersionID < vs[j].VersionID })

	oldest := vs[0]
	latest := vs[len(vs)-1]

	p := Plan{Current: current, Target: latest.VersionID}

	switch {
	case current == latest.VersionID:
		p.Mode = ModeUpToDate
		p.Reason = "версия совпадает с последней публикацией"
		return p, nil

	case current > latest.VersionID:
		// Наша версия новее доступных. Так бывает при откате публикации на
		// стороне ФНС. Молча «обновляться назад» нельзя: это откатило бы
		// справочник, и никто бы не заметил.
		p.Mode = ModeUpToDate
		p.Reason = fmt.Sprintf(
			"наша версия %d новее последней доступной %d — вероятен откат публикации, требуется решение человека",
			current, latest.VersionID)
		return p, nil

	case current <= 0:
		p.Mode = ModeFull
		p.FullURL = latest.FullURL
		p.Reason = "справочник ещё не собирался"
		return p, nil

	case current < oldest.VersionID:
		p.Mode = ModeFull
		p.FullURL = latest.FullURL
		p.Reason = fmt.Sprintf(
			"наша версия %d старше самой старой доступной %d: дельты за этот промежуток уже не публикуются",
			current, oldest.VersionID)
		return p, nil
	}

	// Обычный случай: набираем дельты новее нашей.
	for _, v := range vs {
		if v.VersionID <= current {
			continue
		}
		if v.DeltaURL == "" {
			// Разрыв в цепочке дельт: пропустить нельзя, значит нужна
			// полная пересборка.
			p.Mode = ModeFull
			p.FullURL = latest.FullURL
			p.Reason = fmt.Sprintf("у версии %d нет дельты, цепочка разорвана", v.VersionID)
			p.Deltas = nil
			return p, nil
		}
		p.Deltas = append(p.Deltas, v)
	}

	if len(p.Deltas) == 0 {
		p.Mode = ModeUpToDate
		p.Reason = "новых версий нет"
		return p, nil
	}

	p.Mode = ModeDelta
	p.Reason = fmt.Sprintf("догоняем %d публикаций дельтами", len(p.Deltas))
	return p, nil
}

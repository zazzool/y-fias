package sync

import (
	"testing"

	"github.com/zazzool/y-fias/internal/index"
)

func snap(streets ...index.Street) *index.Snapshot {
	return index.Build(streets, 1)
}

// Главное правило синхронизации: переименование улицы в ГАР НЕ переписывает
// наш адрес, а создаёт запись для человека. Инвариант 1 действует и здесь.
func TestRenameDoesNotTouchOurAddress(t *testing.T) {
	before := snap(
		index.Street{Name: "Старая", Type: "улица", Region: "77", District: "Басманный"},
	)
	after := snap(
		index.Street{Name: "Новая", Type: "улица", Region: "77", District: "Басманный"},
	)

	ours := []OurAddress{
		{ID: "a1", Street: "Старая улица", House: "7", District: "Басманный"},
		{ID: "a2", Street: "Старая улица", House: "9", District: "Басманный"},
	}

	rep := Reconcile(ours, before, after)

	var renames []Change
	for _, c := range rep.Changes {
		if c.Kind == ChangeRenamed {
			renames = append(renames, c)
		}
	}
	if len(renames) != 1 {
		t.Fatalf("переименований %d, ожидалось 1: %+v", len(renames), rep.Changes)
	}

	c := renames[0]
	if c.OldValue != "Старая улица" || c.NewValue != "Новая улица" {
		t.Errorf("получено %q -> %q", c.OldValue, c.NewValue)
	}
	if len(c.AddressIDs) != 2 {
		t.Errorf("затронутых адресов %d, ожидалось 2", len(c.AddressIDs))
	}

	// И главное: сами адреса остались как были. Reconcile ничего не меняет,
	// поэтому проверяем, что вход не тронут.
	for _, a := range ours {
		if a.Street != "Старая улица" {
			t.Errorf("адрес %s изменён: %q", a.ID, a.Street)
		}
	}
}

// Упразднённая улица не удаляется: туда выезжала бригада.
func TestRetiredStreetIsKept(t *testing.T) {
	before := snap(
		index.Street{Name: "Снесённая", Type: "улица", Region: "77", District: "Донской"},
	)
	// В новой версии нет ничего — и подходящего кандидата на переименование
	// тоже нет.
	after := snap()

	ours := []OurAddress{{ID: "a1", Street: "Снесённая улица", District: "Донской"}}
	rep := Reconcile(ours, before, after)

	var retired int
	for _, c := range rep.Changes {
		if c.Kind == ChangeRetired {
			retired++
			if len(c.AddressIDs) != 1 || c.AddressIDs[0] != "a1" {
				t.Errorf("не указан затронутый адрес: %+v", c)
			}
		}
	}
	if retired != 1 {
		t.Errorf("упразднений %d, ожидалось 1: %+v", retired, rep.Changes)
	}
}

// Смена района — повод для ревью, а не для тихой правки: от района зависит ПНД.
func TestDistrictChangeIsReported(t *testing.T) {
	before := snap(
		index.Street{Name: "Общая", Type: "улица", Region: "77", District: "Старый"},
	)
	after := snap(
		index.Street{Name: "Общая", Type: "улица", Region: "77", District: "Новый"},
	)

	ours := []OurAddress{{ID: "a1", Street: "Общая улица", District: "Старый"}}
	rep := Reconcile(ours, before, after)

	var found bool
	for _, c := range rep.Changes {
		if c.Kind == ChangeDistrict {
			found = true
			if c.OldValue != "Старый" || c.NewValue != "Новый" {
				t.Errorf("получено %q -> %q", c.OldValue, c.NewValue)
			}
		}
	}
	if !found {
		t.Errorf("смена района не замечена: %+v", rep.Changes)
	}
}

// Совпадающие данные не должны порождать ни одной записи на ревью: иначе
// очередь заполнится шумом и человек перестанет её читать.
func TestNoChangesWhenNothingChanged(t *testing.T) {
	s := []index.Street{
		{Name: "Тверская", Type: "улица", Region: "77", District: "Тверской"},
		{Name: "Саляма Адиля", Type: "улица", Region: "77", District: "Хорошёво-Мнёвники"},
	}
	before, after := snap(s...), snap(s...)

	ours := []OurAddress{
		{ID: "a1", Street: "Тверская улица", District: "Тверской"},
		{ID: "a2", Street: "улица Саляма Адиля", District: "Хорошёво-Мнёвники"},
	}

	rep := Reconcile(ours, before, after)
	if len(rep.Changes) != 0 {
		t.Errorf("на неизменившихся данных %d записей на ревью: %+v",
			len(rep.Changes), rep.Changes)
	}
	if rep.MatchedGar != 2 {
		t.Errorf("найдено в справочнике %d из 2", rep.MatchedGar)
	}
	if rep.MatchRate() != 1 {
		t.Errorf("MatchRate = %.2f, ожидалась 1", rep.MatchRate())
	}
}

// Разное написание одной улицы не должно считаться расхождением: ключ улицы
// на то и нужен.
func TestSpellingIsNotAChange(t *testing.T) {
	s := []index.Street{{Name: "Саляма Адиля", Type: "улица", Region: "77", District: "Хорошёво-Мнёвники"}}
	before, after := snap(s...), snap(s...)

	ours := []OurAddress{
		{ID: "a1", Street: "Саляма Адиля ул.", District: "Хорошёво-Мнёвники"},
		{ID: "a2", Street: "улица Саляма Адиля", District: "Хорошево-Мневники"}, // ё/е
	}

	rep := Reconcile(ours, before, after)
	if len(rep.Changes) != 0 {
		t.Errorf("разное написание принято за расхождение: %+v", rep.Changes)
	}
	if rep.MatchedGar != 2 {
		t.Errorf("найдено %d из 2 — ключ улицы не сработал", rep.MatchedGar)
	}
}

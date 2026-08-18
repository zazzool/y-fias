package sync

import (
	"testing"
	"time"

	"github.com/zazzool/y-fias/internal/index"
)

func TestSmokeTestCatchesBrokenSnapshot(t *testing.T) {
	// Справочник, где заведомо существующие адреса не находятся, подменять
	// нельзя, даже если по числу улиц он выглядит целым. Именно так
	// проявляется ошибка слияния: она не падает, она молча оставляет
	// неполные данные.
	broken := index.Build([]index.Street{
		{Name: "Совершенно посторонняя", Type: "улица", Region: "77"},
	}, 1)

	if err := smokeTest(broken); err == nil {
		t.Error("справочник без известных адресов прошёл проверку")
	}

	good := index.Build([]index.Street{
		{Name: "Саляма Адиля", Type: "улица", Region: "77"},
		{Name: "Тверская", Type: "улица", Region: "77"},
		{Name: "Ленинский", Type: "проспект", Region: "77"},
		{Name: "Профсоюзная", Type: "улица", Region: "77"},
	}, 1)

	if err := smokeTest(good); err != nil {
		t.Errorf("исправный справочник не прошёл проверку: %v", err)
	}
}

// Планировщик не должен блокировать вызывающего и обязан переживать
// остановку без запущенной синхронизации.
func TestSchedulerStartStop(t *testing.T) {
	snap := index.Build([]index.Street{
		{Name: "Тверская", Type: "улица", Region: "77"},
	}, 1)

	s := NewScheduler(SchedulerOptions{
		Holder:   index.NewHolder(snap),
		SnapPath: "нет-такого-файла.yfsnap",
		// Большой период: тест не должен ждать настоящей синхронизации.
		Interval: time.Hour,
	})

	s.Start()

	// Trigger не блокирует, даже если прогон уже запрошен.
	done := make(chan struct{})
	go func() {
		for range 100 {
			s.Trigger()
		}
		close(done)
	}()

	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("Trigger заблокировался")
	}

	s.Stop()

	st := s.Status()
	t.Logf("состояние после остановки: %+v", st)
}

// Отсутствие справочника — не повод падать: сервис продолжает отвечать на
// том, что уже загружено в память.
func TestSchedulerSurvivesMissingSnapshot(t *testing.T) {
	snap := index.Build([]index.Street{
		{Name: "Тверская", Type: "улица", Region: "77"},
	}, 1)

	s := NewScheduler(SchedulerOptions{
		Holder:   index.NewHolder(snap),
		SnapPath: "нет-такого-файла.yfsnap",
		Interval: time.Hour,
	})

	// runOnce вызывается напрямую, без цикла: проверяем именно обработку
	// ошибки, а не расписание.
	err := s.runOnce(t.Context())
	if err == nil {
		t.Error("ожидалась ошибка чтения справочника")
	}

	// Справочник в памяти не пострадал.
	if s.holder.Load().Len() != 1 {
		t.Error("неудачная синхронизация испортила загруженный справочник")
	}
}

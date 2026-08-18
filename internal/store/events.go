package store

import (
	"context"
	"encoding/json"
	"log/slog"
	"sync"
	"time"

	"github.com/jackc/pgx/v5"
)

// EventKind — вид события журнала.
type EventKind string

const (
	EventTyped     EventKind = "typed"     // человек печатал
	EventSuggested EventKind = "suggested" // мы показали подсказки
	EventChosen    EventKind = "chosen"    // человек выбрал вариант
	EventRejected  EventKind = "rejected"  // человек стёр подставленное
	EventCorrected EventKind = "corrected" // человек исправил
	EventResolved  EventKind = "resolved"  // адрес записан в карту
	EventMerged    EventKind = "merged"    // адреса слиты
	EventShadow    EventKind = "shadow"    // сравнение с прежней системой
)

// Event — запись журнала.
type Event struct {
	Kind        EventKind
	SessionID   string
	RawInput    string
	Suggestions any
	ChosenKey   string
	FinalStreet string
	FinalHouse  string
	SnapshotVer int64
	WeightsVer  int
	TookMs      float64
	Meta        any
}

// Journal — асинхронная запись событий.
//
// Требование, определяющее устройство: журнал НЕ ДОЛЖЕН влиять на время
// ответа. Подсказка обязана уложиться в 5 мс, а запись в базу занимает
// миллисекунды и зависит от чужой доступности. Поэтому события кладутся в
// буфер и пишутся пачками в отдельной горутине.
//
// Следствие, которое надо принять сознательно: при переполнении буфера или
// падении процесса события ТЕРЯЮТСЯ. Это допустимо — журнал служит обучению и
// разбору, а не учёту. Терять подсказки ради полноты журнала было бы
// неправильным разменом.
type Journal struct {
	store *Store
	log   *slog.Logger

	ch     chan Event
	wg     sync.WaitGroup
	closed chan struct{}

	// Счётчики для метрик: молча теряемые события должны быть видимы.
	mu      sync.Mutex
	written int64
	dropped int64
}

// JournalOptions управляет буфером и пачками.
type JournalOptions struct {
	// BufferSize — сколько событий помещается в очередь до потерь.
	BufferSize int
	// BatchSize — сколько событий пишется одним запросом.
	BatchSize int
	// FlushInterval — как часто сбрасывается неполная пачка.
	FlushInterval time.Duration
	Log           *slog.Logger
}

func NewJournal(s *Store, opt JournalOptions) *Journal {
	if opt.BufferSize <= 0 {
		opt.BufferSize = 4096
	}
	if opt.BatchSize <= 0 {
		opt.BatchSize = 128
	}
	if opt.FlushInterval <= 0 {
		opt.FlushInterval = 2 * time.Second
	}
	if opt.Log == nil {
		opt.Log = slog.Default()
	}

	j := &Journal{
		store:  s,
		log:    opt.Log,
		ch:     make(chan Event, opt.BufferSize),
		closed: make(chan struct{}),
	}

	j.wg.Add(1)
	go j.loop(opt.BatchSize, opt.FlushInterval)
	return j
}

// Write кладёт событие в очередь. НИКОГДА не блокирует.
func (j *Journal) Write(e Event) {
	if j == nil {
		return
	}
	select {
	case j.ch <- e:
	default:
		// Очередь полна. Роняем событие и считаем — но не ждём.
		j.mu.Lock()
		j.dropped++
		j.mu.Unlock()
	}
}

// Stats — сколько записано и сколько потеряно.
func (j *Journal) Stats() (written, dropped int64) {
	if j == nil {
		return 0, 0
	}
	j.mu.Lock()
	defer j.mu.Unlock()
	return j.written, j.dropped
}

// Close дописывает остаток и останавливает запись.
func (j *Journal) Close() {
	if j == nil {
		return
	}
	close(j.ch)
	j.wg.Wait()
}

func (j *Journal) loop(batchSize int, flush time.Duration) {
	defer j.wg.Done()

	batch := make([]Event, 0, batchSize)
	ticker := time.NewTicker(flush)
	defer ticker.Stop()

	writeBatch := func() {
		if len(batch) == 0 {
			return
		}
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		err := j.insert(ctx, batch)
		cancel()

		j.mu.Lock()
		if err != nil {
			j.dropped += int64(len(batch))
		} else {
			j.written += int64(len(batch))
		}
		j.mu.Unlock()

		if err != nil {
			// Ошибка записи журнала НЕ должна выглядеть как авария сервиса:
			// подсказки продолжают работать. Но и молчать нельзя.
			j.log.Warn("журнал событий не записан",
				"событий", len(batch), "ошибка", err)
		}
		batch = batch[:0]
	}

	for {
		select {
		case e, ok := <-j.ch:
			if !ok {
				writeBatch()
				return
			}
			batch = append(batch, e)
			if len(batch) >= batchSize {
				writeBatch()
			}
		case <-ticker.C:
			writeBatch()
		}
	}
}

func (j *Journal) insert(ctx context.Context, batch []Event) error {
	rows := make([][]any, 0, len(batch))
	for _, e := range batch {
		rows = append(rows, []any{
			string(e.Kind),
			nullableUUID(e.SessionID),
			nullable(e.RawInput),
			toJSON(e.Suggestions),
			nullable(e.ChosenKey),
			nullable(e.FinalStreet),
			nullable(e.FinalHouse),
			nullableInt64(e.SnapshotVer),
			nullableInt(e.WeightsVer),
			e.TookMs,
			toJSON(e.Meta),
		})
	}

	_, err := j.store.pool.CopyFrom(ctx,
		pgx.Identifier{"addr_events"},
		[]string{
			"kind", "session_id", "raw_input", "suggestions", "chosen_key",
			"final_street", "final_house", "snapshot_ver", "weights_ver",
			"took_ms", "meta",
		},
		pgx.CopyFromRows(rows))
	return err
}

func toJSON(v any) any {
	if v == nil {
		return nil
	}
	b, err := json.Marshal(v)
	if err != nil {
		return nil
	}
	return string(b)
}

func nullable(s string) any {
	if s == "" {
		return nil
	}
	return s
}

func nullableUUID(s string) any {
	if s == "" {
		return nil
	}
	return s
}

func nullableInt64(v int64) any {
	if v == 0 {
		return nil
	}
	return v
}

func nullableInt(v int) any {
	if v == 0 {
		return nil
	}
	return v
}

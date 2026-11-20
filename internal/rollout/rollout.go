// Package rollout — флаги переключения чтения на Y-FIAS (этап 4 плана).
//
// МИС спрашивает у сервиса, какую долю запросов каждого метода обслуживать
// ответами Y-FIAS, и сама решает для конкретного запроса по ключу (обычно —
// идентификатору диспетчера). Решение детерминировано: один и тот же ключ
// при той же доле всегда попадает в одну сторону, а при росте доли с 10% до
// 50% попавшие в 10% остаются внутри. Диспетчер не прыгает между двумя
// источниками посреди смены.
//
// Главное свойство — откат. Снижение доли применяется в памяти немедленно и
// не зависит от базы: если PostgreSQL недоступен, откат всё равно работает,
// а ответ честно говорит, что после перезапуска вернётся прежнее значение.
package rollout

import (
	"context"
	"errors"
	"fmt"
	"hash/fnv"
	"maps"
	"slices"
	"strconv"
	"strings"
	"sync"
	"time"
)

// Методы, которые переключаются по отдельности.
//
// Запись (resolve, merge) сюда намеренно не входит: это этап 5, и
// переключать её долями нельзя — два источника тождества одновременно дают
// дубли адресов.
const (
	Suggest  = "suggest"
	Parse    = "parse"
	Validate = "validate"
	Geo      = "geo"
)

// Endpoints — все переключаемые методы в постоянном порядке.
var Endpoints = []string{Suggest, Parse, Validate, Geo}

// HashName — название алгоритма распределения, отдаётся клиенту, чтобы
// смена алгоритма не прошла незамеченной.
const HashName = "fnv1a32-mod100"

// Bucket — корзина ключа для метода, от 0 до 99.
//
// FNV-1a от «метод:ключ». Метод входит в хеш, чтобы при одинаковых долях
// разные методы включались у разных диспетчеров: иначе первые 10% получили
// бы все новшества сразу, и по жалобе нельзя было бы понять, в чём дело.
//
// Алгоритм повторён в клиенте МИС (docs/07-rollout.md) и закреплён общими
// контрольными значениями в тесте. Менять его — значит перетасовать всех
// диспетчеров разом.
func Bucket(endpoint, key string) int {
	h := fnv.New32a()
	_, _ = h.Write([]byte(endpoint))
	_, _ = h.Write([]byte{':'})
	_, _ = h.Write([]byte(key))
	return int(h.Sum32() % 100)
}

// Serves сообщает, обслуживается ли запрос с ключом ответом Y-FIAS.
//
// Без ключа запрос уходит в прежнюю систему, пока доля не стала полной:
// случайный выбор без ключа заставил бы одного диспетчера видеть то один,
// то другой список подсказок.
func Serves(percent int, endpoint, key string) bool {
	switch {
	case percent >= 100:
		return true
	case percent <= 0, key == "":
		return false
	default:
		return Bucket(endpoint, key) < percent
	}
}

// Change — одно изменение доли.
type Change struct {
	Endpoint string    `json:"endpoint"`
	From     int       `json:"from"`
	To       int       `json:"to"`
	Reason   string    `json:"reason,omitempty"`
	Actor    string    `json:"actor,omitempty"`
	At       time.Time `json:"at"`
}

// Rollback — снижение доли.
func (c Change) Rollback() bool { return c.To < c.From }

// Persister хранит доли между перезапусками.
type Persister interface {
	LoadRollout(ctx context.Context) (map[string]int, error)
	SaveRollout(ctx context.Context, c Change) error
}

// State — текущие доли.
type State struct {
	Flags   map[string]int `json:"flags"`
	Updated time.Time      `json:"updated_at"`
}

// Ошибки проверки. Отделены, чтобы API отвечал на них 400, а не 500.
var (
	ErrUnknownEndpoint = errors.New("неизвестный метод")
	ErrPercentRange    = errors.New("доля вне 0..100")
	ErrReasonRequired  = errors.New("увеличение доли без причины")
)

// Registry — доли в памяти процесса.
//
// Чтение идёт на каждый запрос флагов от МИС, запись — руками администратора
// раз в несколько дней, отсюда RWMutex.
type Registry struct {
	mu      sync.RWMutex
	flags   map[string]int
	updated time.Time
	store   Persister
	now     func() time.Time
}

// New создаёт реестр. Методы, не упомянутые в defaults, выключены: без
// явного решения трафик идёт в прежнюю систему.
func New(defaults map[string]int, store Persister) *Registry {
	flags := make(map[string]int, len(Endpoints))
	for _, e := range Endpoints {
		flags[e] = 0
	}
	for e, p := range defaults {
		if _, ok := flags[e]; ok {
			flags[e] = clamp(p)
		}
	}
	return &Registry{flags: flags, store: store, now: time.Now}
}

// Load подтягивает сохранённые доли поверх значений запуска.
//
// Сохранённое важнее флага командной строки: откат, сделанный руками,
// не должен отменяться перезапуском контейнера с прежней конфигурацией.
func (r *Registry) Load(ctx context.Context) error {
	if r.store == nil {
		return nil
	}
	saved, err := r.store.LoadRollout(ctx)
	if err != nil {
		return err
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	for e, p := range saved {
		if _, ok := r.flags[e]; ok {
			r.flags[e] = clamp(p)
		}
	}
	return nil
}

// State возвращает копию текущих долей.
func (r *Registry) State() State {
	r.mu.RLock()
	defer r.mu.RUnlock()
	return State{Flags: maps.Clone(r.flags), Updated: r.updated}
}

// Percent — доля одного метода.
func (r *Registry) Percent(endpoint string) int {
	r.mu.RLock()
	defer r.mu.RUnlock()
	return r.flags[endpoint]
}

// SetResult — итог изменения.
type SetResult struct {
	Change Change
	// Persisted — изменение записано в базу и переживёт перезапуск.
	Persisted bool
	// SaveErr — почему не записано (база не подключена или недоступна).
	SaveErr error
}

// Set меняет долю метода.
//
// Порядок важен: сначала память, потом база. Откат обязан сработать, даже
// когда база лежит, — именно в такие моменты он чаще всего и нужен.
//
// Увеличение требует причины, снижение — нет: откат делают в спешке, и
// любая преграда на этом пути вредна, а повышение должно быть осознанным
// решением, которое потом можно прочитать в журнале.
func (r *Registry) Set(ctx context.Context, endpoint string, percent int, reason, actor string) (SetResult, error) {
	if !slices.Contains(Endpoints, endpoint) {
		return SetResult{}, fmt.Errorf("%w %q", ErrUnknownEndpoint, endpoint)
	}
	if percent < 0 || percent > 100 {
		return SetResult{}, fmt.Errorf("%w: %d", ErrPercentRange, percent)
	}

	r.mu.Lock()
	from := r.flags[endpoint]
	if percent > from && strings.TrimSpace(reason) == "" {
		r.mu.Unlock()
		return SetResult{}, fmt.Errorf("%w: %s %d%% → %d%%", ErrReasonRequired, endpoint, from, percent)
	}
	now := r.now()
	r.flags[endpoint] = percent
	r.updated = now
	r.mu.Unlock()

	c := Change{
		Endpoint: endpoint, From: from, To: percent,
		Reason: strings.TrimSpace(reason), Actor: strings.TrimSpace(actor), At: now,
	}
	res := SetResult{Change: c}

	if r.store == nil {
		res.SaveErr = errors.New("база не подключена")
		return res, nil
	}
	// Ошибка записи — не ошибка изменения: доля уже действует, и вызывающий
	// узнаёт о несохранённости из SaveErr, а не из отказа.
	res.SaveErr = r.store.SaveRollout(ctx, c)
	res.Persisted = res.SaveErr == nil
	return res, nil
}

// ParseSpec разбирает значения запуска вида «suggest=10,validate=100».
func ParseSpec(spec string) (map[string]int, error) {
	out := map[string]int{}
	for part := range strings.SplitSeq(spec, ",") {
		part = strings.TrimSpace(part)
		if part == "" {
			continue
		}
		name, val, ok := strings.Cut(part, "=")
		if !ok {
			return nil, fmt.Errorf("ожидается метод=доля, получено %q", part)
		}
		name = strings.TrimSpace(name)
		if !slices.Contains(Endpoints, name) {
			return nil, fmt.Errorf("%w %q", ErrUnknownEndpoint, name)
		}
		p, err := strconv.Atoi(strings.TrimSpace(strings.TrimSuffix(val, "%")))
		if err != nil {
			return nil, fmt.Errorf("доля %q: %w", val, err)
		}
		if p < 0 || p > 100 {
			return nil, fmt.Errorf("%w: %s=%d", ErrPercentRange, name, p)
		}
		out[name] = p
	}
	return out, nil
}

func clamp(p int) int { return max(0, min(100, p)) }

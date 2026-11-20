# Переключение чтения: флаги и раскатка

Этап 4 плана. Бэкенд МИС начинает **показывать** ответы Y-FIAS для `suggest`,
`parse`, `validate` и `geo`, сначала части диспетчеров, затем всем. Запись
(`getOrCreate`, слияние) по-прежнему идёт старым путём: это этап 5.

Главное требование — **откат флагом, без выкладки**, и он должен работать
даже тогда, когда что-то сломалось.

---

## Как устроено

```
админка МИС ──PUT /v1/rollout──▶ Y-FIAS: доля в памяти → база (+ журнал)
                                    │
бэкенд МИС ──GET /v1/rollout──▶ ────┘   кэш 15 с
     │
     └─ для запроса: bucket(метод, ключ диспетчера) < доля ?
            да  → ответ Y-FIAS (при ошибке/таймауте → прежний путь)
            нет → прежний путь (+ теневая отправка, как на этапе 3)
```

Доли хранит Y-FIAS, а решение по конкретному запросу принимает МИС. Так
Y-FIAS не стоит на пути запроса при доле 0%, а откат не требует выкладки
МИС.

### Свойства, на которые можно опираться

| Свойство | Почему важно |
|---|---|
| По умолчанию все методы выключены (0%) | без явного решения ничего не меняется |
| Распределение детерминировано по ключу | диспетчер не видит то один, то другой список посреди смены |
| Рост доли не выбрасывает уже включённых | 10% входят в 50%, раскатка без перетасовки |
| Методы делят диспетчеров независимо | по жалобе видно, какой метод виноват |
| Откат применяется мгновенно, даже без базы | откат нужнее всего, когда что-то лежит |
| Повышение требует причины, откат нет | откат делают в спешке; повышение должно читаться в журнале |
| Сохранённое значение важнее флага запуска | ручной откат не отменяется перезапуском контейнера |
| Y-FIAS недоступен → всё идёт по прежнему пути | в том числе при недоступных флагах |

---

## API

### `GET /v1/rollout` — доли (вызывает бэкенд МИС)

```json
{
  "flags": {"suggest": 10, "parse": 0, "validate": 100, "geo": 0},
  "updated_at": "2026-11-20T12:00:00+03:00",
  "hash": "fnv1a32-mod100",
  "ttl_seconds": 15
}
```

`ttl_seconds` — сколько держать в кэше. Это и есть время отката: после
снижения доли все экземпляры МИС увидят его не позже чем через 15 секунд.

`hash` — алгоритм распределения. Если значение не `fnv1a32-mod100`, клиент
обязан считать все доли нулевыми: он делит диспетчеров не так, как сервер.

### `PUT /v1/rollout` — изменить долю (только админка МИС)

```json
{"endpoint": "suggest", "percent": 50, "reason": "неделя на 10% без жалоб", "actor": "ivanov"}
```

Ответ — новые доли плюс `change`, `persisted` и, если изменение не
сохранено в базе, `warning`. **Предупреждение показывать администратору
обязательно**: изменение действует, но после перезапуска сервиса вернётся
прежнее значение.

Ошибки (400): неизвестный метод (в том числе попытка переключить запись),
доля вне 0..100, повышение без `reason`, пропущенное поле `percent`.

### `GET /v1/rollout/history?limit=100` — журнал изменений

Новые первыми. По нему проверяется условие выхода этапа: «неделя на 100% без
отката». Без подключённой базы отвечает 503.

Как и разбор теневых расхождений, `PUT` и `history` вызываются **только**
через авторизованный маршрут админки МИС: у самого сервиса аутентификации нет.

### Метрики

```
yfias_rollout_percent{endpoint="suggest"}         текущая доля
yfias_rollout_rollbacks_total{endpoint="suggest"} откаты с момента запуска
```

---

## Что добавить в МИС

### `server/utils/yfiasRollout.ts`

```ts
/**
 * Переключение чтения на Y-FIAS (этап 4).
 *
 * Правило: любой сбой Y-FIAS — недоступность, таймаут, мусор в ответе, —
 * означает прежний путь. Диспетчер не должен заметить, что Y-FIAS лёг.
 */
const YFIAS_URL = process.env.YFIAS_URL || ''
const FLAGS_TIMEOUT_MS = 200
const CALL_TIMEOUT_MS = 300
const HASH = 'fnv1a32-mod100'

export type Endpoint = 'suggest' | 'parse' | 'validate' | 'geo'

let cache: { flags: Record<string, number>; until: number } | null = null

async function flags(): Promise<Record<string, number>> {
  if (!YFIAS_URL) return {}
  if (cache && cache.until > Date.now()) return cache.flags
  try {
    const res = await fetch(`${YFIAS_URL}/v1/rollout`, {
      signal: AbortSignal.timeout(FLAGS_TIMEOUT_MS),
    })
    const body = await res.json()
    // Чужой алгоритм деления — значит, мы делим не так, как сервер.
    const f = res.ok && body.hash === HASH ? body.flags : {}
    cache = { flags: f, until: Date.now() + (body.ttl_seconds ?? 15) * 1000 }
    return f
  } catch {
    // Флаги недоступны — всё по прежнему пути. Кэшируем отказ ненадолго,
    // чтобы лежащий Y-FIAS не опрашивался на каждый запрос.
    cache = { flags: {}, until: Date.now() + 5000 }
    return {}
  }
}

/** Корзина 0..99. Тот же алгоритм, что internal/rollout.Bucket. */
export function bucket(endpoint: string, key: string): number {
  const bytes = new TextEncoder().encode(`${endpoint}:${key}`)
  let h = 0x811c9dc5
  for (const b of bytes) {
    h ^= b
    h = Math.imul(h, 0x01000193) >>> 0
  }
  return h % 100
}

export async function servedByYfias(endpoint: Endpoint, key: string): Promise<boolean> {
  const p = (await flags())[endpoint] ?? 0
  if (p >= 100) return true
  if (p <= 0 || !key) return false
  return bucket(endpoint, key) < p
}

/**
 * Ответ Y-FIAS для включённых диспетчеров, прежний путь для остальных и
 * при любой ошибке. key — идентификатор диспетчера (пользователя), не сессии:
 * один человек должен видеть один источник всю смену.
 */
export async function withYfias<T>(
  endpoint: Endpoint,
  key: string,
  body: unknown,
  adapt: (yfias: any) => T,
  legacy: () => Promise<T>,
): Promise<{ result: T; source: 'yfias' | 'legacy' }> {
  if (await servedByYfias(endpoint, key)) {
    try {
      const res = await fetch(`${YFIAS_URL}/v1/${endpoint}`, {
        method: 'POST',
        headers: { 'Content-Type': 'application/json' },
        body: JSON.stringify(body),
        signal: AbortSignal.timeout(CALL_TIMEOUT_MS),
      })
      if (res.ok) return { result: adapt(await res.json()), source: 'yfias' }
    } catch {
      // падаем на прежний путь
    }
  }
  return { result: await legacy(), source: 'legacy' }
}
```

### Контрольные значения

Проверьте `bucket` в тесте МИС на тех же числах, что стоят в
`internal/rollout/rollout_test.go`. Расхождение означает, что доля на
графиках Y-FIAS не совпадает с долей на деле.

| endpoint | key | bucket |
|---|---|---:|
| `suggest` | `""` | 77 |
| `suggest` | `disp-1` | 63 |
| `suggest` | `disp-2` | 82 |
| `validate` | `disp-1` | 45 |
| `geo` | `42` | 82 |
| `parse` | `иванова` | 67 |

### Точки вызова

| Где | Что |
|---|---|
| `addressService.suggest` | `withYfias('suggest', userId, {query, region, patient_streets}, …)` |
| `addressService.parse` / разбор строки | `withYfias('parse', userId, {address, house}, …)` |
| определение района | `withYfias('geo', userId, {street, house, district}, …)` |
| SPA, поле адреса | новый вызов `validate`; сегодня его нет вовсе (§8.8 описи) |

Теневую отправку (`shadowCompare`) **не убирать**: для запросов, ушедших по
прежнему пути, она продолжает копить сравнение. Для ушедших в Y-FIAS
сравнивать не с чем, и это нормально.

`source` из `withYfias` полезно писать в журнал запроса МИС: при жалобе
диспетчера первым делом нужно знать, чей ответ он видел.

---

## Порядок раскатки

Порядок как в §8.1 описи: **сначала код-потребитель, затем переключение
источника**. Код из этого документа выкладывается при всех долях 0% и ничего
не меняет. Только после этого доли поднимаются в админке.

| Шаг | Доля | Условие перехода к следующему |
|---|---:|---|
| 0 | 0% | код МИС выложен, `GET /v1/rollout` виден в логах Y-FIAS |
| 1 | 10% | сутки без жалоб и 5xx, `yfias_errors_total` не растёт |
| 2 | 50% | три дня, `correction_rate` у включённых не выше, чем у остальных |
| 3 | 100% | неделя без отката — условие выхода этапа |

`validate` можно включать сразу на 100%: в SPA его сейчас нет вовсе, и
сравнивать не с чем, а выключенный флаг скрывает предупреждения, как и
сегодня.

Откат на любом шаге — `PUT` с `percent: 0`, без причины, действует в течение
15 секунд.

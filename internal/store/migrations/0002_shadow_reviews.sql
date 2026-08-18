-- +goose Up
-- Разбор теневых расхождений.
--
-- Условие выхода этапа 3 — «расхождений класса worse ноль» — недостижимо без
-- места, где человек отмечает разобранное. Часть worse окажется ложной
-- (диспетчер выбрал не то, что искал; выбор из чипа истории; опечатка в самом
-- выборе), и без пометки такие случаи вечно держали бы счётчик ненулевым, а
-- значит обесценили бы само условие.
--
-- Журнал addr_events при этом НЕ переписывается: он сырой и неизменяемый
-- (§8.1 архитектуры). Решение человека — отдельный факт, живущий отдельно.
CREATE TABLE IF NOT EXISTS shadow_reviews (
    event_id    bigint PRIMARY KEY REFERENCES addr_events (id) ON DELETE CASCADE,

    -- open        — расхождение видно, но не разобрано;
    -- accepted    — разобрано и принято как наша недоработка (чинить нас);
    -- fixed       — исправлено, ждём подтверждения новыми данными;
    -- false_alarm — не расхождение: выбор человека был случайным или ошибочным.
    status      text        NOT NULL CHECK (status IN ('open', 'accepted', 'fixed', 'false_alarm')),

    -- Пояснение обязательно по смыслу, но не по схеме: пустая отметка
    -- «разобрано» без причины — это не разбор, однако запрещать её на уровне
    -- базы значило бы мешать работе там, где причина очевидна из запроса.
    note        text,
    reviewer    text,
    reviewed_at timestamptz NOT NULL DEFAULT now()
);

CREATE INDEX IF NOT EXISTS idx_shadow_reviews_status ON shadow_reviews (status);

-- +goose Down
DROP TABLE IF EXISTS shadow_reviews;

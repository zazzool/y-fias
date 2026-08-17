-- Расширения, нужные для контрактных тестов и для воспроизведения
-- поисковых планов прода (§5 инвентаризации: триграммные GIN-индексы).
CREATE EXTENSION IF NOT EXISTS pg_trgm;
CREATE EXTENSION IF NOT EXISTS btree_gin;

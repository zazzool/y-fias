#!/usr/bin/env bash
#
# Этап 0.2: выгрузка golden-корпуса с прода.
#
# ТОЛЬКО SELECT. Скрипт не имеет права ничего менять на проде — если вы
# правите его и добавляете что-то кроме SELECT, вы делаете это не в том файле.
#
# Персональные данные не выгружаются: берутся справочные поля адресов и
# агрегированные счётчики, без идентификаторов пациентов и текстов вызовов.
# Результат попадает в testdata/golden/ и НЕ коммитится (.gitignore) —
# в репозиторий идёт только манифест с контрольными суммами.
#
# Использование:
#   scripts/dump-golden.sh
#   YFIAS_PROD_HOST=root@1.2.3.4 scripts/dump-golden.sh

set -euo pipefail

HOST="${YFIAS_PROD_HOST:-root@PROD_HOST}"
CONTAINER="${YFIAS_PROD_CONTAINER:-ponp-postgres}"
DB_USER="${YFIAS_PROD_USER:-ponp}"
DB_NAME="${YFIAS_PROD_DB:-ponp}"
OUT="${YFIAS_GOLDEN_DIR:-testdata/golden}"

mkdir -p "$OUT"

# Выполнить SELECT на проде и положить результат в CSV.
#   $1 — имя файла без расширения
#   $2 — SQL (обязан начинаться с SELECT или WITH)
dump() {
	local name="$1" sql="$2"

	case "${sql#"${sql%%[![:space:]]*}"}" in
		SELECT*|WITH*) ;;
		*) echo "ОТКАЗ: '$name' — запрос не начинается с SELECT/WITH" >&2; exit 1 ;;
	esac

	printf '  %-22s' "$name"
	ssh -o BatchMode=yes "$HOST" \
		"docker exec -i $CONTAINER psql -U $DB_USER -d $DB_NAME \
		 --csv -v ON_ERROR_STOP=1 -c \"${sql//\"/\\\"}\"" \
		> "$OUT/$name.csv"
	printf '%s строк\n' "$(( $(wc -l < "$OUT/$name.csv") - 1 ))"
}

echo "Выгрузка golden-корпуса с $HOST -> $OUT"
echo

# ── Основной корпус ──────────────────────────────────────────────

dump addresses "
	SELECT id, region, district, street, house, organization, pnd_id
	FROM addresses ORDER BY id"

# Материал контрактного теста Go <-> SQL (§4.1 архитектуры).
dump street_keys "
	SELECT DISTINCT street, street_key(street) AS key, search_norm(street) AS norm
	FROM addresses WHERE street IS NOT NULL AND street <> '' ORDER BY street"

# 2 034 уникальных написания — golden-корпус разбора дома.
dump houses "
	SELECT DISTINCT house, house_num(house) AS num
	FROM addresses WHERE house IS NOT NULL AND house <> '' ORDER BY house"

# Частоты для ранжирования (§5.3 архитектуры). Только счётчики.
dump freq "
	SELECT address_id, count(*) AS calls
	FROM calls WHERE address_id IS NOT NULL GROUP BY address_id ORDER BY 2 DESC"

dump corrections "SELECT * FROM address_corrections ORDER BY id"
dump pnds        "SELECT * FROM pnds ORDER BY id"

# ── Материал регресс-тестов ──────────────────────────────────────

# 81 группа настоящих дублей: совпадает и улица, и полный номер дома.
dump dup_groups "
	SELECT street_key(street) AS key, house, count(*) AS n,
	       string_agg(id, ',' ORDER BY id) AS ids,
	       string_agg(DISTINCT street, ' | ') AS variants
	FROM addresses WHERE house IS NOT NULL AND house <> ''
	GROUP BY 1, 2 HAVING count(*) > 1 ORDER BY 3 DESC"

# 1 922 группы «улица + ЧИСЛО дома» — это РАЗНЫЕ корпуса.
# Слить их нельзя: ровно на этом сломалась старая система (§8.2).
dump numgroups "
	SELECT street_key(street) AS key, house_num(house) AS num, count(*) AS n,
	       string_agg(DISTINCT house, ',' ORDER BY house) AS houses
	FROM addresses WHERE house IS NOT NULL AND house <> ''
	GROUP BY 1, 2 HAVING count(*) > 1 ORDER BY 3 DESC"

# 181 улица, дома которой лежат в разных районах: на них вывод района
# «по соседям» уверенно врёт (§2.3, §10.4).
dump cross_district "
	SELECT street_key(street) AS key, count(DISTINCT district) AS districts,
	       string_agg(DISTINCT district, ' | ') AS names
	FROM addresses WHERE district IS NOT NULL AND district <> ''
	GROUP BY 1 HAVING count(DISTINCT district) > 1 ORDER BY 2 DESC"

# ── Измерение дефекта, найденного на этапе 0.3 ───────────────────
#
# normalizeStreetForCompare() (копия C) не отбрасывает типы улиц из-за \b
# с кириллицей, поэтому SQL находит кандидата, а JS его отвергает, и
# создаётся дубликат. См. testdata/reference/README.md §1.3.
#
# Здесь считается размер дефекта: один street_key и один номер дома, но
# РАЗНОЕ написание улицы.
dump dup_by_streetkey "
	SELECT street_key(street) AS key, house, count(DISTINCT street) AS spellings,
	       string_agg(DISTINCT street, ' | ') AS variants,
	       string_agg(id, ',' ORDER BY id) AS ids
	FROM addresses WHERE house IS NOT NULL AND house <> ''
	GROUP BY 1, 2 HAVING count(DISTINCT street) > 1 ORDER BY 3 DESC"

# ── Грязные данные ───────────────────────────────────────────────

# 21 адрес с литеральной строкой 'null null' — след старого импорта.
dump garbage "
	SELECT id, street, house, district FROM addresses
	WHERE street IS NULL OR street = '' OR street ILIKE '%null%'
	   OR house ILIKE '%null%' ORDER BY id"

# ── Сверка с инвентаризацией ─────────────────────────────────────

dump counters "
	SELECT 'addresses' AS metric, count(*) AS value FROM addresses
	UNION ALL SELECT 'with_district', count(*) FROM addresses WHERE district <> ''
	UNION ALL SELECT 'with_pnd',      count(*) FROM addresses WHERE pnd_id IS NOT NULL
	UNION ALL SELECT 'with_org',      count(*) FROM addresses WHERE organization <> ''
	UNION ALL SELECT 'region_77',     count(*) FROM addresses WHERE region = '77'
	UNION ALL SELECT 'region_other',  count(*) FROM addresses WHERE region <> '77'
	UNION ALL SELECT 'with_coords',   count(*) FROM addresses WHERE lat IS NOT NULL
	UNION ALL SELECT 'cyrillic_house',count(*) FROM addresses WHERE house ~ '[а-яА-ЯёЁ]'
	UNION ALL SELECT 'distinct_house',count(DISTINCT house) FROM addresses WHERE house <> ''
	ORDER BY 1"

# ── Манифест ─────────────────────────────────────────────────────

echo
echo "Контрольные суммы -> $OUT/MANIFEST.txt"
{
	echo "# golden-корпус, снят $(date -u +%Y-%m-%dT%H:%M:%SZ) с $HOST"
	echo "# строки  sha256  файл"
	for f in "$OUT"/*.csv; do
		printf '%s  %s  %s\n' \
			"$(( $(wc -l < "$f") - 1 ))" \
			"$(sha256sum "$f" | cut -c1-16)" \
			"$(basename "$f")"
	done
} > "$OUT/MANIFEST.txt"

echo
echo "=== Сверка с инвентаризацией (address-subsystem-inventory.md §2.3) ==="
awk -F, 'NR>1 {print "  " $1 ": " $2}' "$OUT/counters.csv"
cat <<'EOF'

Ожидается по описи:
  addresses 10694 · with_district 10382 · with_pnd 1684 · with_org 396
  region_77 10649 · region_other 42 · with_coords 0
  cyrillic_house 6036 · distinct_house 2034

Расхождение означает, что опись устарела. Разберитесь ДО этапа 1:
индекс и тесты строятся на этих числах.
EOF

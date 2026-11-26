# Пилот выкатки Y-FIAS

Стенд использует проект Compose `yfias-release-pilot`, закрытую сеть, отдельный PostgreSQL и вымышленный справочник. Команды рассчитаны на Linux с Docker Compose, Bash, Python 3, jq и util-linux. Запуск из каталога репозитория. Рабочий контур описан в [основной инструкции](../../docs/06-deploy.md).

## Подготовка

`stage` — отдельный каталог, доступный для записи только оператору Docker. В `repository` лежит разрешённое полное имя OCI-репозитория без тега. Сборщику нужна запись в этот репозиторий, оператору стенда — чтение. PostgreSQL берётся по закреплённому digest.

```bash
install -d -m 0700 "$stage" "$stage/snapshots"
printf '%s\n' "$repository" > "$stage/repository"
go run ./deploy/staging/fixture "$stage/fixture.yfsnap"
snapshot_sha=$(sha256sum "$stage/fixture.yfsnap" | cut -d ' ' -f 1)
install -m 0444 "$stage/fixture.yfsnap" "$stage/snapshots/$snapshot_sha.yfsnap"
```

Создайте отдельный секрет Lockbox для стенда с полями `postgres-password`, `yfias-dsn`, `yfias-keys`, `probe-client`, `probe-admin`. Пароль из строки подключения должен совпадать с паролем стендовой базы. Строки в `yfias-keys` содержат роли `client`, `admin` и SHA-256 соответствующих ключей. Восстановление читает закреплённую версию и передаёт её прямо в защищённый каталог.

```bash
set -o pipefail
yc lockbox payload get --id "$secret_id" --version-id "$version_id" --format json |
  python3 deploy/staging/materialize-secrets.py restore "$stage/secrets"
```

Для автономной репетиции без Lockbox команда `python3 deploy/staging/materialize-secrets.py generate "$stage/secrets"` создаёт случайные стендовые ключи и пароль. Оба режима отказываются перезаписывать существующие файлы. Сохраняйте версию секрета рядом с журналом выкатки. Смена пароля существующей базы требует отдельной операции в PostgreSQL.

## Выпуск и восстановление

```bash
image=$(bash deploy/staging/build-image.sh "$repository")
bash deploy/staging/release.sh "$stage" "$image" "$snapshot_sha"
```

Сборка читает только файлы текущего коммита. Приёмка проверяет доступ с двумя ролями, результат поиска и запись в PostgreSQL. Только после этого выкатка меняет запись `current`. При ошибке она пробует принятую пару образа и снимка. Без ранее принятой пары она убирает контейнер приложения, сохраняя базу. После прерванной выкатки та же команда с одним аргументом `"$stage"` повторяет восстановление из `pending`.

Сохраняйте принятую пару в реестре, на диске и в локальном кэше Docker, пока она нужна для восстановления. Это возвращает бинарник и снимок. Изменения схемы PostgreSQL проходят отдельную проверку совместимости развёртываемой и предыдущей версий. Перед переносом в МИС нужны репетиция с тестовой копией схемы, проверка восстановления базы и отдельная приёмка на действующем хосте.

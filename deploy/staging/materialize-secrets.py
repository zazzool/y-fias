"""Подготовка секретов из payload Lockbox. Стендовый генератор создаёт только тестовые данные."""

import hashlib
import json
import os
import secrets
import sys
import tempfile
from pathlib import Path

NAMES = {"postgres-password", "yfias-dsn", "yfias-keys", "probe-client", "probe-admin"}


def generate():
    password = secrets.token_hex(32)
    client = "yf_" + secrets.token_urlsafe(32)
    admin = "yf_" + secrets.token_urlsafe(32)
    keyring = "".join(
        f"pilot-{role} {role} sha256:{hashlib.sha256(key.encode()).hexdigest()}\n"
        for role, key in (("client", client), ("admin", admin))
    )
    return {
        "postgres-password": password,
        "yfias-dsn": f"postgres://yfias:{password}@db:5432/yfias?sslmode=disable",
        "yfias-keys": keyring,
        "probe-client": client,
        "probe-admin": admin,
    }


def restore():
    entries = json.load(sys.stdin)["entries"]
    values = {entry["key"]: entry["text_value"] for entry in entries}
    if len(entries) != len(NAMES) or values.keys() != NAMES:
        raise ValueError
    if any(not isinstance(value, str) or not value.strip() or "\x00" in value
           for value in values.values()):
        raise ValueError
    return values


if len(sys.argv) != 3 or sys.argv[1] not in {"generate", "restore"}:
    raise SystemExit("Укажите generate или restore и путь к новому каталогу секретов")

target = Path(sys.argv[2])
if target.exists() or target.is_symlink():
    raise SystemExit("Каталог секретов уже существует")

try:
    values = generate() if sys.argv[1] == "generate" else restore()
    os.umask(0o077)
    with tempfile.TemporaryDirectory(dir=target.parent) as temp:
        source = Path(temp) / "payload"
        source.mkdir(mode=0o700)
        for name, value in values.items():
            path = source / name
            path.write_text(value, encoding="utf-8")
            if name in {"yfias-dsn", "yfias-keys"}:
                os.chown(path, 0, 10001)
                path.chmod(0o440)
        # Postgres читает пароль из файла до смены UID. Дочитывание после
        # смены пользователя требует доступа для postgres (UID/GID 70 в Alpine).
        path = source / "postgres-password"
        os.chown(path, 0, 70)
        path.chmod(0o440)
        source.rename(target)
except (OSError, ValueError, KeyError, TypeError):
    raise SystemExit("Не удалось подготовить полный набор стендовых секретов") from None

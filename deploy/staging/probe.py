"""Приёмка работает внутри сетевого пространства контейнера без внешнего порта."""

import json
import sys
import time
from pathlib import Path
from urllib.error import HTTPError, URLError
from urllib.request import ProxyHandler, Request, build_opener

http = build_opener(ProxyHandler({}))
secrets = Path(sys.argv[1])
client = (secrets / "probe-client").read_text().strip()
admin = (secrets / "probe-admin").read_text().strip()


def call(path, key=None, payload=None, expected=200):
    headers = {"Content-Type": "application/json"}
    if key:
        headers["Authorization"] = f"Bearer {key}"
    data = json.dumps(payload).encode() if payload is not None else None
    request = Request(f"http://127.0.0.1:8080{path}", data=data, headers=headers)
    try:
        with http.open(request, timeout=3) as response:
            if response.status == expected:
                return json.load(response)
    except HTTPError as error:
        if error.code == expected:
            return None
        raise SystemExit(f"{path}: HTTP {error.code}, ожидался {expected}") from None
    except (URLError, TimeoutError):
        raise SystemExit(f"{path}: сервис не ответил") from None
    raise SystemExit(f"{path}: ожидался HTTP {expected}")


suggest = {"query": "пилотная", "limit": 2}
call("/v1/suggest", payload=suggest, expected=401)
if call("/healthz")["snapshot_version"] != 20261126:
    raise SystemExit("сервис загрузил другой снимок")
items = call("/v1/suggest", client, suggest)["items"]
if not items or items[0]["street"] != "Пилотная":
    raise SystemExit("сервис не нашёл стендовую улицу")

report = "/v1/shadow/report?days=1"
call(report, client, expected=403)
before = call(report, admin)["totals"]["total"]
call("/v1/shadow", client, {
    "method": "suggest", "query": "пилотная", "legacy": ["Пилотная улица"],
})
for _ in range(20):
    if call(report, admin)["totals"]["total"] > before:
        print("Справочник, права доступа и запись журнала проверены")
        break
    time.sleep(0.5)
else:
    raise SystemExit("стендовая запись не появилась в журнале")

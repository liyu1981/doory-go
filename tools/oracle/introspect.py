#!/usr/bin/env python
"""Ledgergen: compiler-derived inventory of the pinned doory backend.

Runs against the pinned oracle (mirror at .upstream/current/backend) with the
test settings (SQLite, no throttle, no provider keys). Emits:

  ledger/routes-v<sha>.json       every HTTP route + websocket route
  ledger/interfaces-v<sha>.json   every model field + serializer field + DRF config

These are denominators, never proof: they define review obligations for the
port (PLAN.md §3). Regenerate with `pnpm generate`; never hand-edit.

Run (from the repo root, after `pnpm oracle:setup`):
  .oracle/venv/bin/python tools/oracle/introspect.py
"""
import importlib
import json
import os
import subprocess
import sys
from pathlib import Path

REPO = Path(__file__).resolve().parents[2]
MIRROR_BACKEND = REPO / ".upstream" / "current" / "backend"
LEDGER = REPO / "ledger"


def pin_short_sha() -> str:
    out = subprocess.check_output(
        ["git", "-C", str(REPO / ".upstream" / "current"), "rev-parse", "HEAD"], text=True
    )
    return out.strip()[:12]


def route_entry(pattern, prefix=""):
    """Recursively flatten a URLResolver tree into (route, name, callback) rows."""
    rows = []
    for pattern_obj in pattern.url_patterns:
        sub = prefix + str(pattern_obj.pattern)
        if hasattr(pattern_obj, "url_patterns"):  # URLResolver (include)
            rows.extend(route_entry(pattern_obj, sub))
            continue
        callback = getattr(pattern_obj, "callback", None)
        cb = ""
        if callback is not None:
            cb = getattr(callback, "__qualname__", None) or getattr(callback, "__name__", str(callback))
            view_class = getattr(callback, "view_class", None)
            if view_class is not None:
                cb = view_class.__module__ + "." + view_class.__qualname__
        rows.append({"route": sub, "name": getattr(pattern_obj, "name", None) or "", "callback": cb})
    return rows


def dump_routes():
    from django.urls import get_resolver

    http = route_entry(get_resolver())
    ws = []
    try:
        routing = importlib.import_module("apps.collab.routing")
        for p in routing.websocket_urlpatterns:
            ws.append({"route": str(p.pattern), "consumer": "apps.collab.consumers.CollabConsumer"})
    except Exception as e:  # realtime disabled is fine; the route table still matters
        ws.append({"error": f"collab routing unavailable: {e!r}"})
    return {"http": http, "websocket": ws}


def dump_models():
    from django.apps import apps as django_apps

    models = []
    for model in sorted(django_apps.get_models(), key=lambda m: (m._meta.app_label, m._meta.model_name)):
        fields = []
        for f in model._meta.get_fields():
            entry = {
                "name": f.name,
                "kind": type(f).__name__,
                "concrete": bool(getattr(f, "concrete", False)),
                "null": bool(getattr(f, "null", False)),
                "unique": bool(getattr(f, "unique", False)),
                "db_column": getattr(f, "db_column", None),
                "max_length": getattr(f, "max_length", None),
                "related_model": (
                    f.related_model._meta.label.lower()
                    if getattr(f, "related_model", None) is not None
                    else None
                ),
                "related_name": getattr(f, "related_accessor_name", None),
                "choices": [list(c) for c in getattr(f, "choices", None) or []],
            }
            fields.append(entry)
        models.append(
            {
                "label": model._meta.label,
                "db_table": model._meta.db_table,
                "abstract": model._meta.abstract,
                "fields": fields,
            }
        )
    return models


def dump_serializers():
    import glob

    serializers = []
    for path in sorted(glob.glob(str(MIRROR_BACKEND / "apps" / "*" / "serializers.py"))):
        app = path.split("/")[-2]
        mod = importlib.import_module(f"apps.{app}.serializers")
        for name in sorted(dir(mod)):
            obj = getattr(mod, name)
            if not isinstance(obj, type) or name.endswith("Serializer") is False:
                continue
            try:
                inst = obj()
                field_items = list(inst.fields.items())
            except Exception as e:
                # Not silently dropped: recorded with an error so the gap is visible.
                serializers.append(
                    {
                        "module": f"apps.{app}.serializers",
                        "class": name,
                        "fields": None,
                        "introspect_error": repr(e),
                    }
                )
                continue
            fields = [
                {
                    "name": fname,
                    "kind": type(f).__name__,
                    "required": bool(getattr(f, "required", False)),
                    "read_only": bool(getattr(f, "read_only", False)),
                    "allow_null": bool(getattr(f, "allow_null", False)),
                }
                for fname, f in field_items
            ]
            serializers.append({"module": f"apps.{app}.serializers", "class": name, "fields": fields})
    return serializers


def dump_drf_config():
    from django.conf import settings

    return {
        "REST_FRAMEWORK": settings.REST_FRAMEWORK,
        "INSTALLED_APPS": list(settings.INSTALLED_APPS),
        "MIDDLEWARE": list(settings.MIDDLEWARE),
        "AUTH_USER_MODEL": settings.AUTH_USER_MODEL,
    }


def main():
    os.environ.setdefault("DJANGO_SETTINGS_MODULE", "doory.settings.test")
    os.chdir(MIRROR_BACKEND)
    sys.path.insert(0, str(MIRROR_BACKEND))
    import django

    django.setup()

    sha = pin_short_sha()
    LEDGER.mkdir(parents=True, exist_ok=True)
    outputs = {
        f"routes-v{sha}.json": dump_routes,
        f"interfaces-v{sha}.json": lambda: {
            "models": dump_models(),
            "serializers": dump_serializers(),
            "config": dump_drf_config(),
        },
    }
    for filename, producer in outputs.items():
        out = LEDGER / filename
        out.write_text(json.dumps(producer(), indent=2, sort_keys=True, default=str) + "\n")
        print(f"ledgergen: wrote {out}")


if __name__ == "__main__":
    main()

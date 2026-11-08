#!/usr/bin/env python3
"""Generate the Claude Desktop gateway profile from models.json.

Usage:
  generate.py                 # print the profile diff, change nothing
  generate.py --apply         # back up the profile and write it
  generate.py --profile PATH  # target a specific configLibrary entry

The script never invents models: it reads ../models.json, keeps only entries
with "available": true, and leaves every key it does not own untouched.

The target profile is the one whose inferenceGatewayBaseUrl points at this
proxy, not whatever configLibrary/_meta.json calls applied: writing the model
list into some other profile (another gateway) is silently useless.
"""
import json
import os
import shutil
import sys
from urllib.parse import urlsplit

HERE = os.path.dirname(os.path.abspath(__file__))
ROOT = os.path.abspath(os.path.join(HERE, "..", "..", ".."))
MODELS = os.path.join(ROOT, "models.json")
CONFIG = os.path.join(ROOT, "config.json")
LIBRARY = os.path.expanduser("~/Library/Application Support/Claude-3p/configLibrary")

LOOPBACK = {"127.0.0.1", "localhost", "::1", "0.0.0.0"}


def proxy_endpoint():
    """The host:port the proxy listens on, as this proxy's own config has it."""
    try:
        with open(CONFIG) as f:
            listen = (json.load(f).get("listen") or "127.0.0.1:4000").strip()
    except OSError:
        listen = "127.0.0.1:4000"
    host, _, port = listen.rpartition(":")
    return host or "127.0.0.1", port or "80"


def endpoint(url):
    """(host, port) of a base URL, with loopback spellings unified."""
    parts = urlsplit(url or "")
    host = (parts.hostname or "").lower()
    if host in LOOPBACK:
        host = "127.0.0.1"
    port = str(parts.port or (443 if parts.scheme == "https" else 80))
    return host, port


def load_models():
    with open(MODELS) as f:
        data = json.load(f)
    models = [m for m in data.get("models", []) if m.get("available")]
    if not models:
        sys.exit("models.json holds no available models; run cmd/genmodels -probe first")
    return models


def profiles():
    """Every configLibrary entry that looks like a profile, by path."""
    try:
        names = os.listdir(LIBRARY)
    except OSError:
        return []
    out = []
    for name in sorted(names):
        if not name.endswith(".json") or name.startswith("_"):
            continue
        path = os.path.join(LIBRARY, name)
        try:
            with open(path) as f:
                data = json.load(f)
        except (OSError, ValueError):
            continue
        if "inferenceGatewayBaseUrl" in data:
            out.append((path, data))
    return out


def active_profile(explicit):
    if explicit:
        return explicit

    want = proxy_endpoint()
    found = [(path, data) for path, data in profiles()
             if endpoint(data.get("inferenceGatewayBaseUrl")) == want]
    if not found:
        listing = ", ".join(f"{os.path.basename(p)} -> {d.get('inferenceGatewayBaseUrl')}"
                            for p, d in profiles()) or "none"
        sys.exit(f"no profile points at the proxy {want[0]}:{want[1]}; "
                 f"profiles: {listing}")

    try:
        with open(os.path.join(LIBRARY, "_meta.json")) as f:
            applied = json.load(f).get("appliedId")
    except (OSError, ValueError):
        applied = None

    for path, data in found:
        if applied and os.path.basename(path) == applied + ".json":
            return path
    if len(found) == 1:
        return found[0][0]
    sys.exit("more than one profile points at the proxy: "
             + ", ".join(os.path.basename(p) for p, _ in found)
             + "; pass --profile PATH")


def routed_models():
    """Models declared by a route in config.json.

    They live on another endpoint rather than in the gateway catalog, so
    models.json never lists them and they have to be added here.
    """
    try:
        with open(CONFIG) as f:
            routes = (json.load(f).get("models") or {}).get("routes") or []
    except OSError:
        return []
    out = []
    for r in routes:
        if not r.get("id") or not r.get("picker"):
            continue
        p = r.get("pricing") or {}
        out.append({
            "id": r["id"],
            "picker": r["picker"],
            "label": r.get("label") or r["picker"],
            "behaves_as": r.get("behaves_as") or "claude-sonnet-5",
            "max_effort": r.get("max_effort") or "low",
            "supports_1m": (r.get("context") or 0) >= 1_000_000,
            "pricing": {
                "input": p.get("input") or 0,
                "output": p.get("output") or 0,
                "cache_read": p.get("cache_read") or 0,
            },
        })
    return out


def default_model_id():
    try:
        with open(CONFIG) as f:
            return (json.load(f).get("models") or {}).get("default")
    except OSError:
        return None


def build(models, default_id=None):
    # The app makes the first inferenceModels entry the default model, so the
    # proxy's configured default has to come first.
    if default_id:
        models = sorted(models, key=lambda m: m["id"] != default_id)
    entries, prices = [], []
    for m in models:
        entry = {
            "name": m["picker"],
            "labelOverride": m.get("label") or m["picker"],
            "behavesAs": m.get("behaves_as", "claude-sonnet-5"),
            "maxEffort": m.get("max_effort", "low"),
        }
        if m.get("supports_1m"):
            entry["supports1m"] = True
        entries.append(entry)

        p = m.get("pricing") or {}
        inp = float(p.get("input") or 0)
        out = float(p.get("output") or 0)
        cache = float(p.get("cache_read") or 0)
        prices.append({
            "name": m["picker"],
            "inputPerMtok": inp,
            "outputPerMtok": out,
            "cacheReadPerMtok": cache,
            # The app's schema has no "unknown"; 1.25x input is the Anthropic
            # convention for a 5-minute cache write.
            "cacheWritePerMtok": round(inp * 1.25, 6),
        })
    return entries, prices


def main():
    args = sys.argv[1:]
    apply = "--apply" in args
    explicit = None
    if "--profile" in args:
        explicit = args[args.index("--profile") + 1]

    models = load_models()
    # Routed models come from config.json: the catalog does not know them.
    known = {m["id"] for m in models}
    models.extend(m for m in routed_models() if m["id"] not in known)
    default = default_model_id()
    entries, prices = build(models, default)
    path = active_profile(explicit)

    # The picker name for the default, for the report.
    pickers = {m["id"]: m["picker"] for m in models}
    default_picker = pickers.get(default)
    if default and not default_picker:
        print(f"warning: config default {default} is not an available model", file=sys.stderr)

    with open(path) as f:
        profile = json.load(f)

    missing = [m["id"] for m in models if not m.get("picker", "").startswith("claude-")]
    if missing:
        sys.exit("these models have no claude- picker: " + ", ".join(missing))

    updated = dict(profile)
    updated["inferenceModels"] = entries
    updated["inferenceModelPricingEnabled"] = True
    updated["inferenceModelPricing"] = prices

    print(json.dumps({"profile": path,
                      "models": len(entries),
                      "default": default_picker or default,
                      "before": len(profile.get("inferenceModels", [])),
                      "after": len(entries)}, indent=2))
    for key in sorted(updated):
        if profile.get(key) != updated[key]:
            old = json.dumps(profile.get(key), ensure_ascii=False)
            new = json.dumps(updated[key], ensure_ascii=False)
            print(f"~ {key}: {old[:120]}{'…' if len(old) > 120 else ''} -> {new[:120]}{'…' if len(new) > 120 else ''}")

    if not apply:
        print("\ndry run; pass --apply to write (a .bak backup is kept)")
        return

    shutil.copy2(path, path + ".bak")
    tmp = path + ".tmp"
    with open(tmp, "w") as f:
        json.dump(updated, f, indent=2, ensure_ascii=False)
        f.write("\n")
    os.replace(tmp, path)
    print(f"\nwrote {path} (backup: {path}.bak)")
    print("Quit Claude Desktop completely and reopen it: the profile is read at launch.")


if __name__ == "__main__":
    main()

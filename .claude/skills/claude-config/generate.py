#!/usr/bin/env python3
"""Generate the Claude Desktop gateway profile from models.json.

Usage:
  generate.py                 # print the profile diff, change nothing
  generate.py --apply         # back up the profile and write it
  generate.py --profile PATH  # target a specific configLibrary entry

The script never invents models: it reads ../models.json, keeps only entries
with "available": true, and leaves every key it does not own untouched.
"""
import json
import os
import shutil
import sys

HERE = os.path.dirname(os.path.abspath(__file__))
MODELS = os.path.abspath(os.path.join(HERE, "..", "..", "..", "models.json"))
LIBRARY = os.path.expanduser("~/Library/Application Support/Claude-3p/configLibrary")


def load_models():
    with open(MODELS) as f:
        data = json.load(f)
    models = [m for m in data.get("models", []) if m.get("available")]
    if not models:
        sys.exit("models.json holds no available models; run cmd/genmodels -probe first")
    return models


def active_profile(explicit):
    if explicit:
        return explicit
    with open(os.path.join(LIBRARY, "_meta.json")) as f:
        meta = json.load(f)
    applied = meta.get("appliedId")
    if not applied:
        sys.exit("configLibrary/_meta.json has no appliedId")
    return os.path.join(LIBRARY, applied + ".json")


def build(models):
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
    entries, prices = build(models)
    path = active_profile(explicit)

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

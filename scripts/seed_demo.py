#!/usr/bin/env python3
"""Seed a FlyLab server with a demo account and a realistic experiment history.

Talks to a running server over HTTP only (python3 stdlib):

    python3 scripts/seed_demo.py --base-url http://127.0.0.1:8080 --password 'demo-password'

It registers --username (or logs in if the account exists or registration is closed), submits
--runs experiments through POST /api/v1/plans/validate + POST /api/v1/jobs, waits for all of
them and exits non-zero unless every job succeeded.

Job submission uses an Idempotency-Key built from the run index and the validated plan hash,
so re-running the script returns the existing jobs instead of duplicating them, and editing an
experiment yields a new key instead of 409 IDEMPOTENCY_CONFLICT. When the existing job of a key
ended failed or cancelled (e.g. WORKER_INTERRUPTED after a service restart), it is resubmitted
under the next "-try<n>" key, up to --max-resubmits times.
"""

import argparse
import json
import sys
import time
import urllib.error
import urllib.request

TERMINAL = {"succeeded", "failed", "cancelled"}
RESUBMIT = {"failed", "cancelled"}

# (title, prompt, experiment_type, activation [(group, rate_hz)], duration_ms, repeats)
# compare_silencing always silences demo_silencing (the most active sugar GRN), so it is paired
# with sugar drive; readout is always mn9 (proboscis-extension motor neuron pair).
EXPERIMENTS = [
    ("Sugar GRN baseline, 50 Hz", "Stimulate sugar GRNs at 50 Hz for 100 ms and record MN9.",
     "single", [("sugar_grn", 50)], 100, 1),
    ("Sugar dose: 20 Hz", "Activate sugar-sensing GRNs at a low 20 Hz for 500 ms; how much does MN9 fire?",
     "single", [("sugar_grn", 20)], 500, 2),
    ("Sugar dose: 100 Hz", "Sugar GRNs at 100 Hz, 300 ms, readout MN9, three repeats for variability.",
     "single", [("sugar_grn", 100)], 300, 3),
    ("Sugar dose: 200 Hz (saturation)", "Drive sugar receptors at the maximum 200 Hz for 200 ms and watch MN9.",
     "single", [("sugar_grn", 200)], 200, 1),
    ("Сахар 80 Гц, 1 с", "Стимулируй сахарные рецепторные нейроны 80 Гц в течение 1000 мс, считываем MN9.",
     "single", [("sugar_grn", 80)], 1000, 1),
    ("Silencing the top sugar GRN", "Compare sugar GRN drive at 50 Hz with and without silencing demo_silencing, 100 ms.",
     "compare_silencing", [("sugar_grn", 50)], 100, 1),
    ("Silencing at strong drive", "Sugar GRNs at 150 Hz, 400 ms: does removing the top sugar neuron change MN9 output?",
     "compare_silencing", [("sugar_grn", 150)], 400, 2),
    ("Подавление одного нейрона, 120 Гц", "Сравни активацию сахарных GRN 120 Гц с подавлением demo_silencing и без него, 600 мс.",
     "compare_silencing", [("sugar_grn", 120)], 600, 2),
    ("Bitter GRN control, 50 Hz", "Stimulate bitter GRNs at 50 Hz for 200 ms; MN9 should stay mostly silent.",
     "single", [("bitter_grn", 50)], 200, 1),
    ("Bitter GRN, 150 Hz", "Bitter receptor neurons at 150 Hz for 500 ms, readout MN9, two repeats.",
     "single", [("bitter_grn", 150)], 500, 2),
    ("Горькое 30 Гц", "Активируй горькие рецепторы на 30 Гц, 300 мс, смотрим на MN9.",
     "single", [("bitter_grn", 30)], 300, 1),
    ("Sugar + bitter mixture", "Co-activate sugar GRNs at 100 Hz and bitter GRNs at 100 Hz for 300 ms; does bitter suppress MN9?",
     "single", [("sugar_grn", 100), ("bitter_grn", 100)], 300, 2),
    ("Sugar + bitter, bitter dominant", "Sugar at 40 Hz together with bitter at 160 Hz, 500 ms, MN9 readout.",
     "single", [("sugar_grn", 40), ("bitter_grn", 160)], 500, 1),
    ("Ir94e GRNs, 60 Hz", "Stimulate Ir94e labellar neurons at 60 Hz for 200 ms and record MN9.",
     "single", [("ir94e", 60)], 200, 1),
    ("Ir94e GRNs, 180 Hz", "Ir94e receptor neurons at 180 Hz, 800 ms, three repeats.",
     "single", [("ir94e", 180)], 800, 3),
    ("Сахар + Ir94e", "Одновременно сахарные GRN 70 Гц и Ir94e 70 Гц, 400 мс, считываем MN9.",
     "single", [("sugar_grn", 70), ("ir94e", 70)], 400, 1),
    ("Sugar + Ir94e with silencing", "Sugar 90 Hz plus Ir94e 90 Hz, compare with silencing demo_silencing, 250 ms.",
     "compare_silencing", [("sugar_grn", 90), ("ir94e", 90)], 250, 1),
    ("Short pulse, 100 ms at 200 Hz", "A short 100 ms burst of sugar GRNs at 200 Hz: is it enough to reach MN9?",
     "single", [("sugar_grn", 200)], 100, 3),
    ("Long run, 1 s silencing", "Sugar GRNs 60 Hz for a full second, with and without demo_silencing.",
     "compare_silencing", [("sugar_grn", 60)], 1000, 1),
    ("Seed robustness check", "Repeat the 50 Hz sugar experiment with a different random seed, 150 ms, three repeats.",
     "single", [("sugar_grn", 50)], 150, 3),
]


class APIError(Exception):
    pass


def call(base, method, path, body=None, token=None, headers=None):
    data = None if body is None else json.dumps(body).encode()
    req = urllib.request.Request(base.rstrip("/") + path, data=data, method=method)
    req.add_header("Accept", "application/json")
    if data is not None:
        req.add_header("Content-Type", "application/json")
    if token:
        req.add_header("Authorization", "Bearer " + token)
    for k, v in (headers or {}).items():
        req.add_header(k, v)
    try:
        with urllib.request.urlopen(req, timeout=60) as resp:
            return resp.status, json.loads(resp.read().decode() or "{}")
    except urllib.error.HTTPError as e:
        raw = e.read().decode(errors="replace")
        try:
            payload = json.loads(raw)
        except json.JSONDecodeError:
            raise APIError(f"{method} {path} -> HTTP {e.code}, non-JSON body: {raw[:300]}")
        return e.code, payload


def error_code(payload):
    return (payload.get("error") or {}).get("code")


def sign_in(base, username, password, display_name):
    status, payload = call(base, "POST", "/api/v1/auth/register",
                           {"username": username, "password": password, "display_name": display_name})
    if status == 201:
        print(f"registered user {username!r}")
        return payload["token"]
    if error_code(payload) not in ("USERNAME_TAKEN", "REGISTRATION_CLOSED"):
        raise APIError(f"register failed: HTTP {status} {payload}")
    status, payload = call(base, "POST", "/api/v1/auth/login", {"username": username, "password": password})
    if status != 200:
        raise APIError(f"login as {username!r} failed: HTTP {status} {payload}")
    print(f"logged in as existing user {username!r}")
    return payload["token"]


def build_plan(i, spec):
    title, prompt, exp_type, activation, duration, repeats = spec
    cycle = i // len(EXPERIMENTS)
    return title if cycle == 0 else f"{title} (rerun {cycle + 1})", prompt, {
        "schema_version": "1.0",
        "dataset_id": "flywire_630",
        "model_id": "shiu_lif_rust",
        "experiment_type": exp_type,
        "activation": [{"selector": {"group_id": g}, "rate_hz": float(r)} for g, r in activation],
        "silencing": [{"selector": {"group_id": "demo_silencing"}}] if exp_type == "compare_silencing" else [],
        "readout": [{"selector": {"group_id": "mn9"}}],
        "duration_ms": float(duration),
        "repeats": repeats,
        "base_seed": 1000 + 37 * i,
        "report_language": "ru" if any("Ѐ" <= ch <= "ӿ" for ch in prompt) else "en",
    }


def main():
    ap = argparse.ArgumentParser(description=__doc__.splitlines()[0])
    ap.add_argument("--base-url", default="http://127.0.0.1:8080")
    ap.add_argument("--username", default="demo")
    ap.add_argument("--password", required=True)
    ap.add_argument("--display-name", default="Demo Scientist")
    ap.add_argument("--runs", type=int, default=20)
    ap.add_argument("--wait-seconds", type=int, default=3600, help="max total time to wait for all jobs")
    ap.add_argument("--max-resubmits", type=int, default=5,
                    help="how often a run whose existing job failed/was cancelled is resubmitted")
    args = ap.parse_args()
    if args.runs < 1:
        ap.error("--runs must be >= 1")
    if args.max_resubmits < 0:
        ap.error("--max-resubmits must be >= 0")

    token = sign_in(args.base_url, args.username, args.password, args.display_name)

    submitted = []  # (job_id, title)
    for i in range(args.runs):
        title, prompt, plan = build_plan(i, EXPERIMENTS[i % len(EXPERIMENTS)])
        status, val = call(args.base_url, "POST", "/api/v1/plans/validate", plan)
        if status != 200:
            raise APIError(f"validate #{i + 1} ({title}) failed: HTTP {status} {val}")
        plan_hash = val.get("plan_hash")
        if not plan_hash:
            raise APIError(f"validate #{i + 1} ({title}) returned no plan_hash: {val}")
        for attempt in range(args.max_resubmits + 1):
            key = f"seed-demo-{i}-{plan_hash[:16]}" + (f"-try{attempt}" if attempt else "")
            status, job = call(args.base_url, "POST", "/api/v1/jobs",
                               {"plan_id": val["plan_id"], "title": title, "prompt": prompt}, token=token,
                               headers={"Idempotency-Key": key})
            if status not in (200, 202):
                raise APIError(f"submit #{i + 1} ({title}) with key {key} failed: HTTP {status} {job}")
            job_obj = job["job"]
            if status == 200 and job_obj["status"] in RESUBMIT:
                print(f"[{i + 1}/{args.runs}] existing {job_obj['job_id']} ended {job_obj['status']} "
                      f"({job_obj.get('error_code')}); resubmitting")
                continue
            break
        else:
            raise APIError(f"run #{i + 1} ({title}): every one of {args.max_resubmits + 1} keys maps to a "
                           f"failed/cancelled job; raise --max-resubmits or investigate {job_obj['job_id']}")
        job_id = job_obj["job_id"]
        submitted.append((job_id, title))
        print(f"[{i + 1}/{args.runs}] {'queued' if status == 202 else 'existing'} {job_id}  {title}")

    deadline = time.time() + args.wait_seconds
    final = {}
    while len(final) < len(submitted):
        if time.time() > deadline:
            raise APIError(f"timed out after {args.wait_seconds}s with {len(submitted) - len(final)} jobs unfinished")
        for job_id, _ in submitted:
            if job_id in final:
                continue
            status, job = call(args.base_url, "GET", f"/api/v1/jobs/{job_id}", token=token)
            if status != 200:
                raise APIError(f"GET job {job_id} failed: HTTP {status} {job}")
            if job["status"] in TERMINAL:
                final[job_id] = job
        if len(final) < len(submitted):
            time.sleep(2)

    print()
    print(f"{'job_id':<14} {'status':<10} {'spikes_A':>9} {'spikes_B':>9}  title")
    ok = True
    for job_id, title in submitted:
        job = final[job_id]
        summary = job.get("summary") or {}
        a = summary.get("total_spikes_A")
        b = summary.get("total_spikes_B")  # null for single-condition runs
        a = "-" if a is None else a
        b = "-" if b is None else b
        note = ""
        if job["status"] != "succeeded":
            ok = False
            note = f"  [{job.get('error_code')}: {job.get('error_message')}]"
        if job.get("summary_error"):
            ok = False
            note += f"  [summary_error: {job['summary_error']}]"
        print(f"{job_id:<14} {job['status']:<10} {a!s:>9} {b!s:>9}  {title}{note}")

    succeeded = sum(1 for j in final.values() if j["status"] == "succeeded")
    print(f"\n{succeeded}/{len(submitted)} jobs succeeded")
    return 0 if ok else 1


if __name__ == "__main__":
    try:
        sys.exit(main())
    except (APIError, urllib.error.URLError) as exc:
        print(f"seed_demo: error: {exc}", file=sys.stderr)
        sys.exit(1)

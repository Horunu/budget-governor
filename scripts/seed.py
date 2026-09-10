#!/usr/bin/env python3
"""Seeds three demo tenants (acme-corp, beta-startup, charity-org), each
with 2-3 agents and a realistic budget, via the control plane's own HTTP
API (not direct DB access) -- exercising the exact same code path a real
operator would use.

All issued API keys are printed to stdout so you can immediately try the
gateway, and also written to scripts/.seed_output.env (gitignored) so
scripts/smoke.sh can reuse them without re-parsing stdout.

Usage:
    python3 scripts/seed.py [--controlplane-url http://localhost:8081]
"""

from __future__ import annotations

import argparse
import json
import os
import sys
import time
import urllib.error
import urllib.request

DEFAULT_CONTROLPLANE_URL = "http://localhost:8081"


def request(method: str, url: str, headers: dict | None = None, body: dict | None = None) -> tuple[int, dict]:
    data = json.dumps(body).encode() if body is not None else None
    req = urllib.request.Request(url, data=data, method=method)
    req.add_header("Content-Type", "application/json")
    for k, v in (headers or {}).items():
        req.add_header(k, v)
    try:
        with urllib.request.urlopen(req, timeout=10) as resp:
            return resp.status, json.loads(resp.read().decode())
    except urllib.error.HTTPError as e:
        try:
            return e.code, json.loads(e.read().decode())
        except Exception:
            return e.code, {}


def wait_for_health(base_url: str, timeout_s: int = 60) -> None:
    deadline = time.time() + timeout_s
    while time.time() < deadline:
        try:
            status, _ = request("GET", f"{base_url}/v1/health")
            if status == 200:
                return
        except Exception:
            pass
        print("waiting for control plane...", file=sys.stderr)
        time.sleep(2)
    raise SystemExit(f"control plane at {base_url} did not become healthy in {timeout_s}s")


# name -> (dollar_limit, refill_per_min, token_limit)
DEMO_TENANTS = {
    "acme-corp": (100.0, 0.50, 5_000_000),
    "beta-startup": (20.0, 0.10, 1_000_000),
    "charity-org": (5.0, 0.025, 250_000),
}

DEMO_AGENTS = {
    "acme-corp": ["support-bot", "billing-summarizer", "sales-assistant"],
    "beta-startup": ["onboarding-bot", "changelog-writer"],
    "charity-org": ["grant-writer", "volunteer-coordinator"],
}


def main() -> None:
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("--controlplane-url", default=os.environ.get("CONTROLPLANE_URL", DEFAULT_CONTROLPLANE_URL))
    args = parser.parse_args()

    platform_token = os.environ.get("PLATFORM_BOOTSTRAP_TOKEN", "dev-only-change-me")
    base_url = args.controlplane_url

    wait_for_health(base_url)

    seeded: dict = {"tenants": {}}

    for name, (dollar_limit, refill_per_min, token_limit) in DEMO_TENANTS.items():
        status, tenant = request(
            "POST", f"{base_url}/v1/tenants",
            headers={"X-Platform-Token": platform_token},
            body={"name": name},
        )
        if status == 409:
            print(f"tenant {name!r} already exists, skipping creation", file=sys.stderr)
            continue
        if status != 201:
            raise SystemExit(f"failed to create tenant {name!r}: {status} {tenant}")

        tenant_id = tenant["id"]
        print(f"\n=== {name} (tenant_id={tenant_id}) ===")

        status, admin_key_resp = request(
            "POST", f"{base_url}/v1/tenants/{tenant_id}/api-keys",
            headers={"X-Platform-Token": platform_token},
            body={"scopes": ["admin"], "label": "seed-admin"},
        )
        if status != 201:
            raise SystemExit(f"failed to issue admin key for {name!r}: {status} {admin_key_resp}")
        admin_key = admin_key_resp["api_key"]
        print(f"  admin key:  {admin_key}")

        status, budget = request(
            "POST", f"{base_url}/v1/budgets/{tenant_id}",
            headers={"Authorization": f"Bearer {admin_key}"},
            body={
                "token_limit": token_limit,
                "dollar_limit": dollar_limit,
                "refill_rate_per_min": refill_per_min,
            },
        )
        if status != 200:
            raise SystemExit(f"failed to set budget for {name!r}: {status} {budget}")
        print(f"  budget:     ${dollar_limit}/day ceiling, ${refill_per_min}/min refill, {token_limit:,} token ceiling")

        agent_keys = {}
        for agent_name in DEMO_AGENTS[name]:
            status, agent = request(
                "POST", f"{base_url}/v1/agents",
                headers={"Authorization": f"Bearer {admin_key}"},
                body={"name": agent_name},
            )
            if status != 201:
                raise SystemExit(f"failed to create agent {agent_name!r} for {name!r}: {status} {agent}")
            agent_keys[agent_name] = {"agent_id": agent["id"], "api_key": agent["api_key"]}
            print(f"  agent {agent_name!r}: agent_id={agent['id']} key={agent['api_key']}")

        seeded["tenants"][name] = {
            "tenant_id": tenant_id,
            "admin_key": admin_key,
            "agents": agent_keys,
        }

    out_path = os.path.join(os.path.dirname(__file__), ".seed_output.json")
    with open(out_path, "w") as f:
        json.dump(seeded, f, indent=2)
    print(f"\nSeed data written to {out_path}")

    env_path = os.path.join(os.path.dirname(__file__), ".seed_output.env")
    with open(env_path, "w") as f:
        for name, data in seeded["tenants"].items():
            env_name = name.upper().replace("-", "_")
            f.write(f"{env_name}_TENANT_ID={data['tenant_id']}\n")
            f.write(f"{env_name}_ADMIN_KEY={data['admin_key']}\n")
            for agent_name, agent_data in data["agents"].items():
                agent_env = agent_name.upper().replace("-", "_")
                f.write(f"{env_name}_{agent_env}_AGENT_ID={agent_data['agent_id']}\n")
                f.write(f"{env_name}_{agent_env}_KEY={agent_data['api_key']}\n")
    print(f"Seed data also written to {env_path} (source it in shell scripts)")


if __name__ == "__main__":
    main()

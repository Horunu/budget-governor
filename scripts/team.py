#!/usr/bin/env python3
"""Manage a team's members and daily spend caps from the command line.

A "member" is an agent in the control plane: one person (or bot) with its
own gateway key and its own cap. The team itself is the tenant, and its
cap is the ceiling for everyone combined.

    python3 scripts/team.py init my-team --daily 100   # once; prints env to export
    python3 scripts/team.py add bob --daily 25         # prints Bob's key + setup
    python3 scripts/team.py cap bob --daily 40         # change a member's cap
    python3 scripts/team.py cap --daily 150            # change the team cap
    python3 scripts/team.py list                       # caps and last-24h spend
    python3 scripts/team.py rotate bob                 # lost key: revoke + reissue
    python3 scripts/team.py remove bob                 # revoke Bob's keys

Env: CONTROLPLANE_URL (default http://localhost:8081), GATEWAY_URL (default
http://localhost:8080, only used in printed setup), BG_TENANT_ID and
BG_ADMIN_KEY (printed by `init`), PLATFORM_BOOTSTRAP_TOKEN (for `init`).
"""

from __future__ import annotations

import argparse
import os
import sys

from seed import request  # same stdlib HTTP helper the seed script uses

CP = os.environ.get("CONTROLPLANE_URL", "http://localhost:8081")
GATEWAY = os.environ.get("GATEWAY_URL", "http://localhost:8080")


def die(msg: str) -> None:
    raise SystemExit(f"error: {msg}")


def call(method: str, path: str, body: dict | None = None, headers: dict | None = None) -> tuple[int, dict]:
    if headers is None:
        key = os.environ.get("BG_ADMIN_KEY") or die("set BG_ADMIN_KEY (printed by `team.py init`)")
        headers = {"Authorization": f"Bearer {key}"}
    return request(method, f"{CP}{path}", headers=headers, body=body)


def tenant_id() -> str:
    return os.environ.get("BG_TENANT_ID") or die("set BG_TENANT_ID (printed by `team.py init`)")


def set_daily_cap(dollars: float, agent_id: str | None = None) -> None:
    if dollars < 0:
        die("--daily must be >= 0")
    # Token bucket: ceiling = one day's allowance, refilled evenly over 24h,
    # so a member who burns it all at 9am gets it back gradually, not at midnight.
    # shortcut: refill_rate_per_min is NUMERIC(14,4), so caps under ~$1/day refill
    # noticeably off; widen the column if tiny caps matter.
    status, resp = call("POST", f"/v1/budgets/{tenant_id()}", {
        "agent_id": agent_id,
        "dollar_limit": dollars,
        "refill_rate_per_min": dollars / 1440,
    })
    if status != 200:
        die(f"setting cap failed: {status} {resp}")


def member(name: str) -> dict:
    status, agents = call("GET", "/v1/agents")
    if status != 200:
        die(f"listing members failed: {status} {agents}")
    for a in agents:
        if a["name"] == name:
            return a
    die(f"no member named {name!r}")


def print_setup(name: str, key: str) -> None:
    print(f"\nSend this to {name} (the key is shown only once):\n")
    print(f"  export OPENAI_BASE_URL={GATEWAY}/v1")
    print(f"  export OPENAI_API_KEY={key}")
    print("\nAny OpenAI-compatible client or agent framework then goes through the gateway.")
    print("See docs/team-setup.md for details.")


def cmd_init(args: argparse.Namespace) -> None:
    platform = {"X-Platform-Token": os.environ.get("PLATFORM_BOOTSTRAP_TOKEN", "dev-only-change-me")}
    status, tenant = call("POST", "/v1/tenants", {"name": args.team}, headers=platform)
    if status != 201:
        die(f"creating team failed: {status} {tenant}")
    status, key = call("POST", f"/v1/tenants/{tenant['id']}/api-keys", {"scopes": ["admin"], "label": "team-cli"}, headers=platform)
    if status != 201:
        die(f"issuing admin key failed: {status} {key}")
    os.environ["BG_TENANT_ID"], os.environ["BG_ADMIN_KEY"] = tenant["id"], key["api_key"]
    set_daily_cap(args.daily)
    print(f"Team {args.team!r} created with a ${args.daily:g}/day team cap. Keep this admin key private:\n")
    print(f"  export BG_TENANT_ID={tenant['id']}")
    print(f"  export BG_ADMIN_KEY={key['api_key']}")


def cmd_add(args: argparse.Namespace) -> None:
    status, agent = call("POST", "/v1/agents", {"name": args.name})
    if status != 201:
        die(f"adding member failed: {status} {agent}")
    set_daily_cap(args.daily, agent["id"])
    print(f"Added {args.name} with a ${args.daily:g}/day cap.")
    print_setup(args.name, agent["api_key"])


def cmd_cap(args: argparse.Namespace) -> None:
    if args.name:
        set_daily_cap(args.daily, member(args.name)["id"])
        print(f"{args.name}: cap set to ${args.daily:g}/day")
    else:
        set_daily_cap(args.daily)
        print(f"team: cap set to ${args.daily:g}/day")
    print("(the gateway picks this up within BUDGET_CACHE_TTL_SECONDS)")


def cmd_list(_: argparse.Namespace) -> None:
    status, agents = call("GET", "/v1/agents")
    if status != 200:
        die(f"listing members failed: {status} {agents}")
    _, budgets = call("GET", f"/v1/budgets/{tenant_id()}")
    _, spend = call("GET", f"/v1/spend/{tenant_id()}?window=24h")
    caps = {b["agent_id"]: b["dollar_limit"] for b in budgets if not b["task_id"]}
    spent = {row["agent_id"]: row["cost_usd"] for row in spend.get("by_agent", [])}

    def fmt(v: float | None) -> str:
        return "-" if v is None else f"${v:,.2f}"

    print(f"{'member':<20} {'status':<8} {'cap/day':>10} {'spent 24h':>10}")
    for a in agents:
        print(f"{a['name']:<20} {'active' if a['active'] else 'removed':<8} {fmt(caps.get(a['id'])):>10} {fmt(spent.get(a['id'], 0.0)):>10}")
    if None in spent:
        print(f"{'(no member)':<20} {'':<8} {'':>10} {fmt(spent[None]):>10}")
    print(f"{'TEAM TOTAL':<20} {'':<8} {fmt(caps.get(None)):>10} {fmt(spend.get('total_cost_usd', 0.0)):>10}")


def cmd_rotate(args: argparse.Namespace) -> None:
    status, resp = call("POST", f"/v1/agents/{member(args.name)['id']}/rotate-key")
    if status != 200:
        die(f"rotating key failed: {status} {resp}")
    print(f"Old key for {args.name} revoked (the gateway stops accepting it within AUTH_CACHE_TTL_SECONDS).")
    print_setup(args.name, resp["api_key"])


def cmd_remove(args: argparse.Namespace) -> None:
    status, resp = call("DELETE", f"/v1/agents/{member(args.name)['id']}")
    if status != 204:
        die(f"removing member failed: {status} {resp}")
    print(f"{args.name}'s keys revoked (effective within AUTH_CACHE_TTL_SECONDS). Spend history is kept.")


def main() -> None:
    p = argparse.ArgumentParser(description=__doc__, formatter_class=argparse.RawDescriptionHelpFormatter)
    sub = p.add_subparsers(dest="cmd", required=True)
    s = sub.add_parser("init", help="create the team and its admin key")
    s.add_argument("team")
    s.add_argument("--daily", type=float, required=True, help="team-wide $/day cap")
    s = sub.add_parser("add", help="add a member with their own key and cap")
    s.add_argument("name")
    s.add_argument("--daily", type=float, required=True, help="member $/day cap")
    s = sub.add_parser("cap", help="change a member's cap, or the team cap if no name")
    s.add_argument("name", nargs="?")
    s.add_argument("--daily", type=float, required=True)
    sub.add_parser("list", help="members, caps and last-24h spend")
    s = sub.add_parser("rotate", help="revoke a member's key and issue a new one")
    s.add_argument("name")
    s = sub.add_parser("remove", help="revoke a member's keys")
    s.add_argument("name")
    args = p.parse_args()
    {"init": cmd_init, "add": cmd_add, "cap": cmd_cap, "list": cmd_list, "rotate": cmd_rotate, "remove": cmd_remove}[args.cmd](args)


if __name__ == "__main__":
    sys.exit(main())

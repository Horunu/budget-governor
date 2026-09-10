"""Long-running entrypoint for the reconciliation container: runs job.py's
reconciliation once at startup (so `make smoke` sees a result immediately
without waiting a day) and then on a daily schedule via APScheduler.

A Kubernetes deployment would more idiomatically run job.py directly as a
CronJob (see the module docstring in job.py) instead of keeping a
container alive between runs; this scheduler exists so `docker compose
up` -- which has no external cron -- still exercises the job on a
recurring basis.
"""

from __future__ import annotations

import asyncio
import logging
import os

from apscheduler.schedulers.asyncio import AsyncIOScheduler
from apscheduler.triggers.cron import CronTrigger

from job import DEFAULT_DRIFT_THRESHOLD_PCT, previous_utc_day, run_from_db

logger = logging.getLogger("reconciliation.scheduler")


async def run_once() -> None:
    dsn = os.environ.get(
        "POSTGRES_DSN", "postgres://budget_governor:budget_governor@localhost:5432/budget_governor"
    )
    threshold_pct = float(os.environ.get("RECONCILIATION_DRIFT_THRESHOLD_PCT", DEFAULT_DRIFT_THRESHOLD_PCT))
    window_start, window_end = previous_utc_day()

    try:
        result = await run_from_db(dsn, window_start, window_end, threshold_pct)
        logger.info(
            "reconciliation run complete: drift_detected=%s total_drift_usd=%.4f tenants=%d",
            result.drift_detected,
            result.total_drift_amount_usd,
            len(result.per_tenant),
        )
    except Exception:
        logger.exception("reconciliation run failed; will retry on next scheduled tick")


async def main() -> None:
    logging.basicConfig(level=logging.INFO)

    # Run immediately at startup, then daily at 01:00 UTC (well after
    # midnight, giving providers' own eventually-consistent usage/cost
    # data a head start on settling for "yesterday").
    await run_once()

    scheduler = AsyncIOScheduler()
    scheduler.add_job(run_once, CronTrigger(hour=1, minute=0))
    scheduler.start()

    logger.info("reconciliation scheduler started, next run scheduled daily at 01:00 UTC")
    await asyncio.Event().wait()  # block forever; the container's lifecycle owns shutdown


if __name__ == "__main__":
    asyncio.run(main())

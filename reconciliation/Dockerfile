# Build context is the repo root (see deploy/docker-compose.yml) so this
# image can COPY the shared providers/ (pricing) package alongside its
# own usage_providers/ (billing API clients).
FROM python:3.12-slim AS base
WORKDIR /app

COPY reconciliation/pyproject.toml ./
COPY reconciliation/job.py reconciliation/scheduler.py ./
COPY reconciliation/usage_providers ./usage_providers
COPY providers ./providers
RUN pip install --no-cache-dir .

CMD ["python", "scheduler.py"]

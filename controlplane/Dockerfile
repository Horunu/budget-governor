FROM python:3.12-slim AS base
WORKDIR /app

RUN apt-get update && apt-get install -y --no-install-recommends gcc libpq-dev && rm -rf /var/lib/apt/lists/*

COPY pyproject.toml ./
COPY app ./app
RUN pip install --no-cache-dir .

EXPOSE 8081
# `python -m uvicorn` so the working directory is on sys.path -- see the
# matching comment in advisor/Dockerfile for why this matters for a
# service that imports a sibling top-level package (not needed here
# today since app/ is pip-installed, but kept consistent).
CMD ["python", "-m", "uvicorn", "app.main:app", "--host", "0.0.0.0", "--port", "8081"]

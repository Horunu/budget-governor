# Build context is the repo root (see deploy/docker-compose.yml) so this
# image can COPY the shared providers/ package alongside its own app/.
FROM python:3.12-slim AS base
WORKDIR /app

COPY advisor/pyproject.toml ./
COPY advisor/app ./app
COPY providers ./providers
RUN pip install --no-cache-dir .

EXPOSE 8082
# `python -m uvicorn` (rather than the bare `uvicorn` console-script)
# specifically so Python prepends the working directory (/app) to
# sys.path, making the providers/ package copied in above importable --
# the installed console-script's own location otherwise would not put
# /app on sys.path.
CMD ["python", "-m", "uvicorn", "app.main:app", "--host", "0.0.0.0", "--port", "8082"]

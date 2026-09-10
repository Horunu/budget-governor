# Build context is the repo root (see deploy/docker-compose.yml) so this
# image can COPY the shared providers/ package alongside its own app/.
FROM python:3.12-slim AS base
WORKDIR /app

COPY advisor/pyproject.toml ./
COPY advisor/app ./app
COPY providers ./providers
RUN pip install --no-cache-dir .

EXPOSE 8082
CMD ["uvicorn", "app.main:app", "--host", "0.0.0.0", "--port", "8082"]

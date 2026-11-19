# k6 evidence

Each run of the `k6-evidence` workflow drops four files here:

- `<stamp>-<tag>-<rate>rps.md`   — summary readable at a glance
- `<stamp>-<tag>-<rate>rps.json` — k6 `--summary-export` payload
- `<stamp>-<tag>-<rate>rps.csv`  — per-request rows for spreadsheet work
- `<stamp>-<tag>-<rate>rps.log`  — raw k6 stdout

Run it manually from the Actions tab after the gateway is up and a tenant
has been seeded. Each run commits its own files to this directory.

"""Puts reconciliation/ and the repo root on sys.path so `import job` and
`from providers.pricing import ...` resolve during local test runs (the
Docker image instead COPYs providers/ alongside job.py -- see
reconciliation/Dockerfile).
"""

import pathlib
import sys

_HERE = pathlib.Path(__file__).resolve().parents[1]
_REPO_ROOT = _HERE.parent
for p in (_HERE, _REPO_ROOT):
    if str(p) not in sys.path:
        sys.path.insert(0, str(p))

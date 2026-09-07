"""Adds the repo root to sys.path so `from providers.pricing import ...`
resolves during local test runs (in the Docker image, providers/ is
copied alongside app/ instead -- see advisor/Dockerfile).
"""

import pathlib
import sys

_REPO_ROOT = pathlib.Path(__file__).resolve().parents[2]
if str(_REPO_ROOT) not in sys.path:
    sys.path.insert(0, str(_REPO_ROOT))

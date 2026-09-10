"""Budget Governor cost advisor: called by the gateway only on budget
pressure (see docs/DECISIONS.md ADR-003). Never the decision-maker --
returns ranked suggestions for the gateway's deterministic policy engine
to evaluate.
"""

from __future__ import annotations

from fastapi import FastAPI

from app.advisor import get_suggestions
from app.config import build_llm_client
from app.schemas import AdviseRequest, AdviseResponse

app = FastAPI(title="Budget Governor Cost Advisor", version="0.1.0")

_llm_client = build_llm_client()


@app.get("/v1/health")
async def health() -> dict:
    return {"status": "ok", "llm_provider": type(_llm_client).__name__}


@app.post("/v1/advise", response_model=AdviseResponse)
async def advise(request: AdviseRequest) -> AdviseResponse:
    return await get_suggestions(request, _llm_client)

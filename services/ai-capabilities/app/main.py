"""Safe mock AI capability service: NO LLM, NO document data access, NO provider calls."""
import hashlib
import hmac
import json
import os
import time
from pathlib import Path
from typing import Any, Literal

from fastapi import FastAPI, HTTPException, Request
from pydantic import BaseModel, ConfigDict, Field

REGISTRY_PATH = Path(os.environ.get('CAPABILITY_REGISTRY_PATH', str(Path(__file__).resolve().parents[3] / 'contracts/capabilities/registry.v1.json')))
REGISTRY = json.loads(REGISTRY_PATH.read_text())
AGENTS = {c['id']: c for c in REGISTRY['capabilities']}
SECRET = os.environ.get('INTERNAL_HMAC_KEY', '')

class Task(BaseModel):
    model_config = ConfigDict(extra='forbid', populate_by_name=True)
    executionId: str
    tenantId: str
    principalId: str
    matterId: str | None = None
    capabilityId: str
    input: dict[str, Any]
    traceId: str

class Evidence(BaseModel):
    model_config = ConfigDict(extra='forbid')
    documentId: str
    documentVersionId: str
    passageId: str
    pageNumber: int | None = None

class Result(BaseModel):
    model_config = ConfigDict(extra='forbid')
    executionId: str
    agentId: str
    agentVersion: str
    status: Literal['completed','abstained','failed']
    output: dict[str, Any]
    evidence: list[Evidence] = Field(default_factory=list)
    proposedActions: list[dict[str, Any]] = Field(default_factory=list)
    requiresAttorneyReview: bool

app = FastAPI(title='LawPilot AI Capability Worker (mock)', docs_url=None, redoc_url=None)

def check_signature(body: bytes, timestamp: str | None, signature: str | None, now_ms: int | None = None) -> bool:
    if len(SECRET) < 32 or not timestamp or not signature or len(timestamp) != 13 or not timestamp.isdigit():
        return False
    now_ms = now_ms if now_ms is not None else int(time.time()*1000)
    if abs(now_ms-int(timestamp))>60_000:
        return False
    expected = hmac.new(SECRET.encode(), timestamp.encode()+b'.'+body, hashlib.sha256).hexdigest()
    return hmac.compare_digest(expected, signature)


def safe_mock(task: Task) -> Result:
    meta = AGENTS.get(task.capabilityId)
    if not meta:
        raise ValueError('UNSUPPORTED_CAPABILITY')
    if meta['requiresMatter'] and not task.matterId:
        raise ValueError('matter_required')
    if len(json.dumps(task.input).encode()) > meta['maxInputBytes']:
        raise ValueError('input_too_large')
    # In later phases: replace these mocks with approved model/provider calls and evidence services.
    if task.capabilityId == 'intake.summarize.v1':
        fields = task.input.get('answers', {})
        if not isinstance(fields, dict):
            raise ValueError('answers_must_be_object')
        status = 'completed' if fields else 'abstained'
        output = {'kind':'mock_intake_validation', 'fieldCount':len(fields),
                  'message':'Synthetic intake fields counted. AI summary and routing are not implemented; attorney review required.'}
    else:
        status = 'abstained'
        output = {'kind':'not_implemented', 'message': (
            'No authorized, verified source evidence or approved template is wired. '
            'Cannot generate a legal summary, cited answer or legal draft.'
        )}
    return Result(executionId=task.executionId, agentId=meta['agentId'], agentVersion=meta['agentVersion'],
                  status=status, output=output, evidence=[], proposedActions=[],
                  requiresAttorneyReview=meta['requiresAttorneyReview'])

@app.get('/healthz')
def health():
    return {'service':'ai-capabilities','status':'up','mode':'safe-mock','capabilities':len(AGENTS)}

@app.post('/internal/execute', response_model=Result, response_model_exclude_none=True)
async def execute(request: Request):
    raw = await request.body()
    if len(raw) > 32_768:
        raise HTTPException(413, 'input_too_large')
    if not check_signature(raw, request.headers.get('x-lawpilot-timestamp'), request.headers.get('x-lawpilot-signature')):
        raise HTTPException(401, 'invalid_internal_signature')
    try:
        task = Task.model_validate_json(raw)
        return safe_mock(task)
    except (ValueError, json.JSONDecodeError):
        raise HTTPException(422, 'invalid_task_or_capability')

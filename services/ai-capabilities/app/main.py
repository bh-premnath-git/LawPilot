"""Safe mock AI capability service: NO LLM, NO document data access, NO provider calls."""
import hashlib
import hmac
import json
import os
import time
from pathlib import Path
from typing import Any, Literal
from uuid import UUID
import re
from contextlib import asynccontextmanager
from opentelemetry import trace, propagate
from opentelemetry.sdk.resources import Resource
from opentelemetry.sdk.trace import TracerProvider
from opentelemetry.sdk.trace.export import BatchSpanProcessor
from opentelemetry.exporter.otlp.proto.http.trace_exporter import OTLPSpanExporter

from fastapi import FastAPI, HTTPException, Request
from pydantic import BaseModel, ConfigDict, Field, model_validator

REGISTRY_PATH = Path(os.environ.get('CAPABILITY_REGISTRY_PATH', str(Path(__file__).resolve().parents[3] / 'contracts/capabilities/registry.v1.json')))
class Capability(BaseModel):
    model_config = ConfigDict(extra='forbid')
    id: str = Field(pattern=r'^[a-z][a-z0-9.]*\.v[1-9]\d*$')
    agentId: str = Field(min_length=1)
    agentVersion: str = Field(pattern=r'^\d+\.\d+\.\d+$')
    requiresMatter: bool
    requiresAttorneyReview: bool
    maxInputBytes: int = Field(gt=0, le=32768)

class Registry(BaseModel):
    model_config = ConfigDict(extra='forbid')
    registryVersion: Literal['1.0']
    capabilities: list[Capability] = Field(min_length=1)

    @model_validator(mode='after')
    def unique_capabilities(self):
        ids=[cap.id for cap in self.capabilities]
        if len(set(ids))!=len(ids):
            raise ValueError('Duplicate capability ID')
        return self

REGISTRY = Registry.model_validate_json(REGISTRY_PATH.read_text())
AGENTS = {cap.id: cap.model_dump() for cap in REGISTRY.capabilities}
SECRET = os.environ.get('INTERNAL_HMAC_KEY', '')

class Task(BaseModel):
    model_config = ConfigDict(extra='forbid', populate_by_name=True)
    executionId: UUID
    tenantId: UUID
    principalId: str = Field(min_length=1)
    matterId: UUID | None = None
    capabilityId: str
    input: dict[str, Any]
    traceId: str = Field(min_length=1)

class Evidence(BaseModel):
    model_config = ConfigDict(extra='forbid')
    documentId: str
    documentVersionId: str
    passageId: str
    pageNumber: int | None = None
    contentSha256: str | None = Field(default=None, pattern=r'^[a-f0-9]{64}$')

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

telemetry_provider = None
if os.environ.get('OTEL_EXPORTER_OTLP_ENDPOINT'):
    telemetry_provider = TracerProvider(resource=Resource.create({'service.name':'ai-capabilities'}))
    telemetry_provider.add_span_processor(BatchSpanProcessor(OTLPSpanExporter()))
    trace.set_tracer_provider(telemetry_provider)

@asynccontextmanager
async def lifespan(_app):
    yield
    if telemetry_provider:
        telemetry_provider.shutdown()

app = FastAPI(telemetry={'tracing':False}, lifespan=lifespan, title='LawPilot AI Capability Worker (mock)', docs_url=None, redoc_url=None)

@app.middleware('http')
async def trace_request(request: Request, call_next):
    # Static name and status only; no legal text, tokens, query strings or tenant labels.
    with trace.get_tracer('lawpilot').start_as_current_span('ai.request', context=propagate.extract(dict(request.headers))) as span:
        response = await call_next(request)
        span.set_attribute('http.response.status_code', response.status_code)
        if response.status_code >= 500:
            span.set_status(trace.Status(trace.StatusCode.ERROR))
        return response

def check_signature(body: bytes, timestamp: str | None, signature: str | None, now_ms: int | None = None) -> bool:
    if len(SECRET) < 32 or not timestamp or not signature or len(timestamp) != 13 or not timestamp.isascii() or not timestamp.isdigit() or not signature.isascii() or not re.fullmatch(r'[a-f0-9]{64}', signature):
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
    if len(json.dumps(task.input, ensure_ascii=False, separators=(',',':')).encode()) > meta['maxInputBytes']:
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
    return Result(executionId=str(task.executionId), agentId=meta['agentId'], agentVersion=meta['agentVersion'],
                  status=status, output=output, evidence=[], proposedActions=[],
                  requiresAttorneyReview=meta['requiresAttorneyReview'])

@app.get('/healthz')
def health():
    return {'service':'ai-capabilities','status':'up','mode':'configured-legal-generation' if os.environ.get('LEGAL_AI_ENABLED')=='true' else 'safe-mock','capabilities':len(AGENTS)}

@app.post('/internal/execute', response_model=Result, response_model_exclude_none=True)
async def execute(request: Request):
    if os.environ.get('LEGAL_AI_ENABLED')=='true' or os.environ.get('ENABLE_LEGACY_INTERNAL_HTTP') != 'true' or os.environ.get('APP_ENV') != 'development':
        raise HTTPException(410, 'use_authenticated_grpc')
    raw = bytearray()
    async for chunk in request.stream():
        if len(raw) + len(chunk) > 32_768:
            raise HTTPException(413, 'input_too_large')
        raw.extend(chunk)
    raw = bytes(raw)
    if not check_signature(raw, request.headers.get('x-lawpilot-timestamp'), request.headers.get('x-lawpilot-signature')):
        raise HTTPException(401, 'invalid_internal_signature')
    try:
        task = Task.model_validate_json(raw)
        return safe_mock(task)
    except (ValueError, json.JSONDecodeError):
        raise HTTPException(422, 'invalid_task_or_capability')

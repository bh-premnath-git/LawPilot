import hashlib
import hmac
import json
import os
import time
from uuid import uuid4

os.environ['ENABLE_LEGACY_INTERNAL_HTTP']='true'
os.environ['APP_ENV']='development'
os.environ['INTERNAL_HMAC_KEY'] = 'unit-test-secret-value-at-least-32-characters'
from fastapi.testclient import TestClient
from app.main import app, check_signature, safe_mock, Task

client=TestClient(app)

def task_for(cap, matter=True):
    x={'executionId':str(uuid4()),'tenantId':str(uuid4()),'principalId':'keycloak-sub',
       'capabilityId':cap,'input':{},'traceId':str(uuid4())}
    if matter: x['matterId']=str(uuid4())
    return x

def signed_post(payload):
    raw=json.dumps(payload).encode()
    ts=str(int(time.time()*1000))
    sig=hmac.new(os.environ['INTERNAL_HMAC_KEY'].encode(),ts.encode()+b'.'+raw,hashlib.sha256).hexdigest()
    return client.post('/internal/execute',content=raw,headers={'x-lawpilot-timestamp':ts,'x-lawpilot-signature':sig})

def test_health():
    assert client.get('/healthz').status_code==200

def test_unsigned_denied():
    assert client.post('/internal/execute',json=task_for('matter.qa.v1')).status_code==401

def test_case_qa_abstains_without_authorized_evidence():
    res=signed_post(task_for('matter.qa.v1'))
    assert res.status_code==200
    data=res.json()
    assert data['status']=='abstained' and data['evidence']==[] and data['proposedActions']==[]

def test_draft_never_claims_approval():
    data=signed_post(task_for('legal.draft.v1')).json()
    assert data['requiresAttorneyReview'] is True
    assert data['status']=='abstained'

def test_intake_safe_mock():
    x=task_for('intake.summarize.v1',matter=False);x['input']={'answers':{'kind':'test','country':'synthetic'}}
    r=signed_post(x)
    assert r.status_code==200
    assert r.json()['output']['fieldCount']==2

def test_unknown_capability_refused():
    assert signed_post(task_for('arbitrary.execute.v1')).status_code==422

def test_signature_rejects_body_tamper():
    assert not check_signature(b'bad','0'*13,'0'*64,int(time.time()*1000))

def test_matter_required():
    assert signed_post(task_for('matter.qa.v1',matter=False)).status_code==422

def test_invalid_identity_and_extra_fields_rejected():
    for change in ({'tenantId':'not-a-uuid'}, {'matterId':''}, {'principalId':''}, {'traceId':''}, {'unexpected':True}):
        task=task_for('matter.qa.v1');task.update(change)
        assert signed_post(task).status_code==422

def test_oversized_body_refused():
    task=task_for('intake.summarize.v1',matter=False)
    task['input']={'answers':{'text':'x'*33000}}
    assert signed_post(task).status_code==413

def test_non_ascii_signature_refused_without_server_error():
    assert not check_signature(b'{}', str(int(time.time()*1000)), 'é'*64)

def test_duplicate_registry_ids_fail_validation():
    from app.main import Registry, REGISTRY
    import pytest
    data=REGISTRY.model_dump()
    data['capabilities'].append(data['capabilities'][0])
    with pytest.raises(ValueError, match='Duplicate capability ID'):
        Registry.model_validate(data)

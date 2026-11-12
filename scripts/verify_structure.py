#!/usr/bin/env python3
"""Dependency-light verification of scaffold integrity; not an integration test."""
from pathlib import Path
import json
import re
root=Path(__file__).resolve().parents[1]
files=[
 'compose.yaml','db/001_init.sql','docs/ARCHITECTURE.md','docs/ROADMAP.md','docs/SECURITY.md',
 'contracts/capabilities/registry.v1.json','contracts/jsonschema/capability-request.v1.json',
 'contracts/jsonschema/agent-result.v1.json','contracts/proto/lawpilot/common/v1/common.proto',
 'apps/platform-api/src/main.ts','services/root-supervisor/src/main.ts',
 'services/ai-capabilities/app/main.py','services/provider-router/main.go',
 'services/workflow-service/workflows/approval.go','services/document-worker/src/main.rs',
]
for name in files:
 assert (root/name).exists(),f'Missing: {name}'
for name in ['contracts/capabilities/registry.v1.json','contracts/jsonschema/capability-request.v1.json','contracts/jsonschema/agent-result.v1.json','infra/keycloak/lawpilot-dev-realm.json']:
 json.loads((root/name).read_text())
registry=json.loads((root/'contracts/capabilities/registry.v1.json').read_text())
ids=[x['id'] for x in registry['capabilities']]
assert len(ids)==len(set(ids)), 'Duplicate capability ID'
assert {'intake.summarize.v1','document.summarize.v1','legal.draft.v1','matter.qa.v1'}==set(ids)
for x in registry['capabilities']:
 assert re.match(r'^[a-z][a-z0-9.]*\.v[1-9]\d*$',x['id']),x['id']
 assert x['maxInputBytes']>0
assert 'clio-manage' in (root/'services/provider-router/main.go').read_text()
assert 'NO LLM' in (root/'services/ai-capabilities/app/main.py').read_text()
print(f'PASS: {len(files)} expected paths, JSON contracts, 4 unique capability IDs, mock safety conditions')

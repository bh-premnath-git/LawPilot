"""Validate local production policy templates; not cluster admission/egress validation."""
from pathlib import Path
import yaml
root=Path(__file__).resolve().parents[1]
certs=list(yaml.safe_load_all((root/'infra/production/certificates.yaml').read_text()))
roles={'api-gateway','execution-worker','root-supervisor','ai-capabilities','calendar-service','context-engine','integration-service','document-parser'}
found=set()
for item in certs:
    if item['kind']=='Certificate':
        role=item['metadata']['name'];found.add(role)
        assert item['spec']['uris']==['spiffe://lawpilot.local/'+role]
        assert item['spec']['commonName']==role
        assert item['spec']['privateKey']['rotationPolicy']=='Always'
assert found==roles
policies=list(yaml.safe_load_all((root/'infra/production/network-policies.yaml').read_text()))
assert any(p['metadata']['name']=='default-deny' and p['spec']['policyTypes']==['Ingress','Egress'] for p in policies)
for p in policies:
    selected=p['spec']['podSelector'].get('matchLabels',{}).get('app.kubernetes.io/name')
    assert not(selected=='document-parser' and p['spec'].get('egress'))
print('PASS: workload certificate scopes and deny-by-default policy templates; cluster validation still required')

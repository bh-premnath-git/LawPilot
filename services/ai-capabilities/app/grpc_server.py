"""mTLS capability boundary: only the Root Supervisor may invoke mock agents."""
import os
import sys
from pathlib import Path
from concurrent.futures import ThreadPoolExecutor
import grpc
import uvicorn
from google.protobuf import json_format

sys.path.insert(0, os.environ.get('RPC_PYTHON_PATH', str(Path(__file__).resolve().parents[3] / 'contracts/gen/python')))
from lawpilot.rpc.v1 import capability_pb2 as messages, capability_pb2_grpc as bindings
from app.main import app, Task
from app.generation import run,config
from opentelemetry import trace, propagate

class CapabilityService(bindings.CapabilityServiceServicer):
    def ExecuteCapability(self, request, context):
        names=context.auth_context().get('x509_common_name', [])
        uri=('spiffe://'+os.environ.get('WORKLOAD_TRUST_DOMAIN','lawpilot.local')+'/root-supervisor').encode()
        sans=context.auth_context().get('x509_subject_alternative_name',[])
        legacy=os.environ.get('APP_ENV')=='development' and os.environ.get('WORKLOAD_ALLOW_LEGACY_CN')=='true'
        if b'root-supervisor' not in names or (uri not in sans and not legacy):
            context.abort(grpc.StatusCode.PERMISSION_DENIED, 'WORKLOAD_SCOPE_DENIED')
        data={'executionId':request.execution_id,'tenantId':request.tenant_id,'principalId':request.principal_id,
              'capabilityId':request.capability_id,'input':json_format.MessageToDict(request.input),'traceId':request.trace_id}
        if request.matter_id:
            data['matterId']=request.matter_id
        try:
            task=Task.model_validate(data)
            with trace.get_tracer('lawpilot').start_as_current_span('capability.execute.grpc',context=propagate.extract(dict(context.invocation_metadata()))):
                result=run(task,json_format.MessageToDict(request.trusted_context) if request.HasField("trusted_context") else None)
            if result.proposedActions:
                raise ValueError('actions_not_allowed')
        except RuntimeError:
            context.abort(grpc.StatusCode.UNAVAILABLE, 'MODEL_PROVIDER_UNAVAILABLE')
        except (ValueError, TypeError, KeyError):
            context.abort(grpc.StatusCode.INVALID_ARGUMENT, 'INVALID_CAPABILITY_REQUEST')
        reply=messages.ExecuteCapabilityResponse(execution_id=result.executionId,agent_id=result.agentId,
            agent_version=result.agentVersion,status=result.status,requires_attorney_review=result.requiresAttorneyReview)
        json_format.ParseDict(result.output, reply.output)
        for reference in result.evidence:
            value=reply.evidence.add(document_id=reference.documentId,document_version_id=reference.documentVersionId,passage_id=reference.passageId)
            if reference.contentSha256 is not None:
                value.content_sha256=reference.contentSha256
            if reference.pageNumber is not None:
                value.page_number=reference.pageNumber
        return reply

def server():
    config()  # Fail closed at startup when enabled configuration is incomplete.
    def read(name):
        return Path(os.environ[name]).read_bytes()
    def certificate_configuration():
        return grpc.ssl_server_certificate_configuration([(read('GRPC_SERVER_KEY_FILE'),read('GRPC_SERVER_CERT_FILE'))],root_certificates=read('GRPC_CA_FILE'))
    def refresh():
        try:return certificate_configuration()
        except (OSError,ValueError):return None
    credentials=grpc.dynamic_ssl_server_credentials(certificate_configuration(),refresh,require_client_authentication=True)
    instance=grpc.server(ThreadPoolExecutor(max_workers=16),maximum_concurrent_rpcs=16,options=[('grpc.max_receive_message_length',1048576),('grpc.max_send_message_length',1048576),('grpc.max_connection_age_ms',300000),('grpc.max_connection_age_grace_ms',30000)])
    bindings.add_CapabilityServiceServicer_to_server(CapabilityService(),instance)
    if not instance.add_secure_port(os.environ.get('AI_GRPC_BIND','0.0.0.0:50053'),credentials):
        raise RuntimeError('Cannot bind mTLS capability service')
    return instance

if __name__=='__main__':
    instance=server();instance.start()
    try:
        uvicorn.run(app,host='0.0.0.0',port=int(os.environ.get('AI_PORT','8000')))
    finally:
        instance.stop(5).wait()

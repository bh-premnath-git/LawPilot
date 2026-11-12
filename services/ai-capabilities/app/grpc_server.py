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
from app.main import app, Task, safe_mock
from opentelemetry import trace, propagate

class CapabilityService(bindings.CapabilityServiceServicer):
    def ExecuteCapability(self, request, context):
        names=context.auth_context().get('x509_common_name', [])
        if b'root-supervisor' not in names:
            context.abort(grpc.StatusCode.PERMISSION_DENIED, 'WORKLOAD_SCOPE_DENIED')
        data={'executionId':request.execution_id,'tenantId':request.tenant_id,'principalId':request.principal_id,
              'capabilityId':request.capability_id,'input':json_format.MessageToDict(request.input),'traceId':request.trace_id}
        if request.matter_id:
            data['matterId']=request.matter_id
        try:
            task=Task.model_validate(data)
            with trace.get_tracer('lawpilot').start_as_current_span('capability.execute.grpc',context=propagate.extract(dict(context.invocation_metadata()))):
                result=safe_mock(task)
            if result.proposedActions:
                raise ValueError('actions_not_allowed')
        except (ValueError, TypeError):
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
    def read(name):
        return Path(os.environ[name]).read_bytes()
    credentials=grpc.ssl_server_credentials([(read('GRPC_SERVER_KEY_FILE'),read('GRPC_SERVER_CERT_FILE'))],root_certificates=read('GRPC_CA_FILE'),require_client_auth=True)
    instance=grpc.server(ThreadPoolExecutor(max_workers=16),options=[('grpc.max_receive_message_length',32768),('grpc.max_send_message_length',1048576)])
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

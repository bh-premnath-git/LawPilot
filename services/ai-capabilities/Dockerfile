FROM python:3.12-slim
WORKDIR /workspace
COPY services/ai-capabilities/requirements.txt /workspace/requirements.txt
RUN pip install --no-cache-dir -r requirements.txt
COPY services/ai-capabilities/app /workspace/app
COPY contracts/gen/python /workspace/contracts/gen/python
COPY contracts/capabilities /workspace/contracts/capabilities
ENV RPC_PYTHON_PATH=/workspace/contracts/gen/python
ENV CAPABILITY_REGISTRY_PATH=/workspace/contracts/capabilities/registry.v1.json
RUN useradd --uid 10001 --no-create-home worker
USER worker
EXPOSE 8000
CMD ["python","-m","app.grpc_server"]

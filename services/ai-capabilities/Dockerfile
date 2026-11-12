FROM python:3.12-slim
WORKDIR /workspace
COPY services/ai-capabilities/requirements.txt /workspace/requirements.txt
RUN pip install --no-cache-dir -r requirements.txt
COPY services/ai-capabilities/app /workspace/app
COPY contracts/capabilities /workspace/contracts/capabilities
ENV CAPABILITY_REGISTRY_PATH=/workspace/contracts/capabilities/registry.v1.json
EXPOSE 8000
CMD ["uvicorn","app.main:app","--host","0.0.0.0","--port","8000"]

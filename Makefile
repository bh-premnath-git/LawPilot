.PHONY: up down logs verify python-test go-test
up:
	docker compose --env-file .env up --build -d

down:
	docker compose down

logs:
	docker compose logs -f --tail=100

verify:
	python3 scripts/verify_structure.py

python-test:
	cd services/ai-capabilities && python3 -m pytest -q

# Five independent Go modules, no go.work -- each needs its own `go test ./...`.
# provider-router is the legacy profiles:[legacy-mock] adapter, kept here only
# because it still has its own tests; the other four are the live services.
go-test:
	cd services/calendar-service && go test ./...
	cd services/context-engine && go test ./...
	cd services/integration-service && go test ./...
	cd services/workflow-service && go test ./...
	cd services/provider-router && go test ./...

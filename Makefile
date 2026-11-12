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

go-test:
	cd services/provider-router && go test ./...

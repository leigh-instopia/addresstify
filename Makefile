.PHONY: build load load-qld serve up down logs clean tidy test-search test-validate test-health

COMPOSE = docker compose -f deploy/docker-compose.yml
PORT   ?= 8080

build:
	$(COMPOSE) build

load:
	$(COMPOSE) --profile loader run --rm loader

load-qld:
	$(COMPOSE) --profile loader run --rm -e GNAF_STATES=QLD loader

serve:
	$(COMPOSE) up api

up:
	$(COMPOSE) --profile loader up

down:
	$(COMPOSE) down

logs:
	$(COMPOSE) logs -f api

test-search:
	curl -s "http://localhost:$(PORT)/addresses?q=1+pitt+street+sydney" | jq .

test-validate:
	curl -s -X POST http://localhost:$(PORT)/addresses/validate \
		-H "Content-Type: application/json" \
		-d '{"number":"1","street_name":"Pitt","street_type":"Street","suburb":"Sydney","state":"NSW","postcode":"2000"}' \
		| jq .

test-health:
	curl -s http://localhost:$(PORT)/health

tidy:
	go mod tidy

clean:
	$(COMPOSE) down -v
	docker image rm gnaf-api gnaf-loader 2>/dev/null || true

.PHONY: help up down psql run test clean

help: ## Show this help
	@grep -E '^[a-zA-Z_-]+:.*?## .*$$' $(MAKEFILE_LIST) | awk 'BEGIN {FS = ":.*?## "}; {printf "\033[36m%-10s\033[0m %s\n", $$1, $$2}'

up: ## Start the Postgres container (host port 5434)
	docker compose up -d --wait

down: ## Stop Postgres and delete its data volume
	docker compose down -v

psql: ## Open a psql shell in the database
	docker compose exec postgres psql -U estoria -d estoria

run: ## Run the ledger rebuild console at http://localhost:8084
	go run .

test: ## Run the tests
	go test -race ./...

clean: ## Remove the built binary
	rm -f ledger

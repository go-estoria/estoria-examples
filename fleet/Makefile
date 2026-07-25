.PHONY: help run test clean

help: ## Show this help
	@grep -E '^[a-zA-Z_-]+:.*?## .*$$' $(MAKEFILE_LIST) | awk 'BEGIN {FS = ":.*?## "}; {printf "\033[36m%-10s\033[0m %s\n", $$1, $$2}'

run: ## Run the fleet dashboard at http://localhost:8083
	go run .

test: ## Run the domain tests
	go test -race ./...

clean: ## Delete the database (the fleet re-registers on next run)
	rm -f fleet.db fleet.db-wal fleet.db-shm

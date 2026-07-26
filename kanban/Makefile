.PHONY: help run test clean

help: ## Show this help
	@grep -E '^[a-zA-Z_-]+:.*?## .*$$' $(MAKEFILE_LIST) | awk 'BEGIN {FS = ":.*?## "}; {printf "\033[36m%-10s\033[0m %s\n", $$1, $$2}'

run: ## Run the kanban board at http://localhost:8080
	go run .

test: ## Run the domain tests
	go test -race ./...

clean: ## Delete the database (the board reseeds on next run)
	rm -f kanban.db kanban.db-wal kanban.db-shm

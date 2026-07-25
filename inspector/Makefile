.PHONY: help run test

help: ## Show this help
	@grep -E '^[a-zA-Z_-]+:.*?## .*$$' $(MAKEFILE_LIST) | awk 'BEGIN {FS = ":.*?## "}; {printf "\033[36m%-10s\033[0m %s\n", $$1, $$2}'

run: ## Run the inspector at http://localhost:8085 (defaults to ../kanban/kanban.db — run the kanban example first)
	go run .

test: ## Run the tests (includes an end-to-end pass over a real SQLite store)
	go test -race ./...

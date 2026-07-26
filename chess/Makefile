.PHONY: help run test clean

help: ## Show this help
	@grep -E '^[a-zA-Z_-]+:.*?## .*$$' $(MAKEFILE_LIST) | awk 'BEGIN {FS = ":.*?## "}; {printf "\033[36m%-10s\033[0m %s\n", $$1, $$2}'

run: ## Run the chess server at http://localhost:8084
	go run .

test: ## Run the domain tests
	go test -race ./...

clean: ## Delete the database (all games are lost)
	rm -f chess.db chess.db-wal chess.db-shm

.DEFAULT_GOAL := help
.PHONY: help up down psql

help: 
	@echo "Available commands:"
	@echo "  up     - Start the PostgreSQL container"
	@echo "  down   - Stop and remove the PostgreSQL container and its volumes"
	@echo "  psql   - Connect to the PostgreSQL database using psql"

# Start the PostgreSQL container in detached mode.
up:
	docker compose up -d mongo mongo-express

# Stop and remove the PostgreSQL container and its volumes.
down:
	docker compose down -v

# Connect to the MongoDB database using mongosh.
mongosh:
	docker compose exec mongo mongosh

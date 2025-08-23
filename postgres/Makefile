.DEFAULT_GOAL := help

help: 
	@echo "Available commands:"
	@echo "  up     - Start the PostgreSQL container"
	@echo "  down   - Stop and remove the PostgreSQL container and its volumes"
	@echo "  psql   - Connect to the PostgreSQL database using psql"

up:
	docker compose up -d postgres

down:
	docker compose down -v

psql:
	docker compose exec postgres psql -U estoria -d estoria

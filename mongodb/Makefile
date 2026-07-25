.DEFAULT_GOAL := help
.PHONY: help up down mongosh

help:
	@echo "Available commands:"
	@echo "  up       - Start the MongoDB and mongo-express containers"
	@echo "  down     - Stop and remove the containers and their volumes"
	@echo "  mongosh  - Open a mongosh shell in the MongoDB container"

# Start the MongoDB containers in detached mode.
up:
	docker compose up -d mongo mongo-express

# Stop and remove the MongoDB containers and their volumes.
down:
	docker compose down -v

# Connect to the MongoDB database using mongosh.
mongosh:
	docker compose exec mongo mongosh

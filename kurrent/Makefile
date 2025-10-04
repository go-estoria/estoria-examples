.DEFAULT_GOAL := help
.PHONY: help up down kurrent

help: 
	@echo "Available commands:"
	@echo "  up        - Start the Kurrent container"
	@echo "  down      - Stop and remove the Kurrent container and its volumes"
	@echo "  kurrent   - Connect to the Kurrent database using psql"

# Start the Kurrent container in detached mode.
up:
	docker compose up -d kurrentdb

# Stop and remove the Kurrent container and its volumes.
down:
	docker compose down -v

# Open the Kurrent web interface in the default web browser.
kurrent:
	open http://localhost:2113

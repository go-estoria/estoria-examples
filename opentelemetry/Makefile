.PHONY: up down

up:
	docker-compose up -d otel-collector jaeger prometheus grafana
	@echo
	@echo "Jaeger UI:     http://localhost:16686"
	@echo "Prometheus UI: http://localhost:9090"
	@echo "Grafana UI:    http://localhost:3000 (u: admin, p: secret)"

down:
	docker-compose down -v

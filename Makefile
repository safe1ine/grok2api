.PHONY: server web build

server:
	cd server && go run ./cmd/grok2api

web:
	cd web && npm run dev

build:
	cd server && go build -o bin/grok2api ./cmd/grok2api
	cd web && npm run build

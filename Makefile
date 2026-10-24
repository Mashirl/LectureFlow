# For environments with make. On Windows use .\make.ps1 (see NOTES.md).
PY ?= .venv/bin/python
.PHONY: build test run realgpu audit
build:
	go build -o lectureflow ./cmd/lectureflow
test:
	go vet ./...
	go test -p 4 ./...
	$(PY) -m pytest -q worker
realgpu:
	go test -p 1 -tags realgpu ./...
	$(PY) -m pytest -q -m realgpu worker
run: build
	./lectureflow -config config.toml
audit:
	go run ./scripts/licaudit

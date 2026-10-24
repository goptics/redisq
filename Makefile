.PHONY: test format diagrams release help

# Default target
help:
	@echo "Available commands:"
	@echo "  make test     - Run all tests with race detection and coverage"
	@echo "  make format   - Format code"
	@echo "  make diagrams - Regenerate the animated README diagrams (assets/diagrams/gen)"
	@echo "  make release  - Create and push a new release tag (usage: make release VERSION=v1.0.0)"

# Run all tests with race detection and coverage
test:
	go test -race -coverprofile=coverage.txt -covermode=atomic -v ./...

# Format code
format:
	gofmt -w .

# Regenerate the animated README diagrams; each generator asserts the behaviour it draws
diagrams:
	cd assets/diagrams/gen && for g in gen_*.py; do python3 "$$g" || exit 1; done

# Create and push a new release tag
release:
ifndef VERSION
	$(error VERSION is required. Usage: make release VERSION=v1.0.0)
endif
	@echo "Creating tag $(VERSION)..."
	git tag -s $(VERSION) -m "Release $(VERSION)"
	git push origin $(VERSION)
	@echo "Tag $(VERSION) pushed. GitHub Actions will handle the rest."

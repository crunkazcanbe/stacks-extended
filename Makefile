# Build the single stacks binary from the bin/lib layout.
build:
	go build -o stacks ./bin/stacks
.PHONY: build

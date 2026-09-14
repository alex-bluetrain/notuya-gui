BINARY := notuya-gui
PKG    := ./cmd/notuya-gui

# GTK4 + gtk4-layer-shell require CGO and the system GTK4 stack.
export CGO_ENABLED := 1

.PHONY: build run test vet check clean deps

build: ## Build the GUI binary (CGO + GTK4)
	go build -o $(BINARY) $(PKG)

run: build ## Build and launch the picker
	./$(BINARY)

test: ## Run unit tests (pure-Go modules)
	go test $(PKG)

vet: ## Static analysis
	go vet $(PKG)

check: vet test ## Vet + test

deps: ## Verify GTK4 toolchain is present
	@pkg-config --exists gtk4 gtk4-layer-shell-0 && echo "toolchain ok" \
		|| { echo "missing gtk4 / gtk4-layer-shell-0 (install gtk4, gtk4-layer-shell)"; exit 1; }

clean: ## Remove the built binary
	rm -f $(BINARY)

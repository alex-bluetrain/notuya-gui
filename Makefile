# GOEXE is empty on Linux and ".exe" under MSYS2, so the Windows build lands
# on notuya-gui.exe without a separate target.
BINARY  := notuya-gui$(shell go env GOEXE)
PKG     := ./cmd/notuya-gui ./internal/...
BINDIR  := $(HOME)/.local/bin

# GTK4 + libadwaita require CGO and the system GTK4 stack.
export CGO_ENABLED := 1

.PHONY: build run test vet check clean deps install uninstall

build: ## Build the GUI binary (CGO + GTK4)
	go build -o $(BINARY) ./cmd/notuya-gui

run: build ## Build and launch the app
	./$(BINARY)

test: ## Run unit tests (pure-Go modules)
	go test $(PKG)

vet: ## Static analysis
	go vet $(PKG)

check: vet test ## Vet + test

deps: ## Verify GTK4 toolchain is present
	@pkg-config --exists gtk4 libadwaita-1 && echo "toolchain ok" \
		|| { echo "missing gtk4 / libadwaita-1 (install gtk4, libadwaita)"; exit 1; }

install: build ## Copy the built binary into ~/.local/bin (overwrites existing)
	@mkdir -p $(BINDIR)
	install -m 0755 $(BINARY) $(BINDIR)/$(BINARY)
	@echo "installed $(BINDIR)/$(BINARY)"

uninstall: ## Remove the installed binary from ~/.local/bin
	rm -f $(BINDIR)/$(BINARY)

clean: ## Remove the built binary
	rm -f $(BINARY)

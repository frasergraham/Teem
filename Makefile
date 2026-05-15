.PHONY: ui build clean test

# Build the SPA bundle that //go:embed in cmd/teem/ui_embed.go picks up.
# Must run before `go build ./cmd/teem` from a clean checkout.
ui:
	cd cmd/teem/ui && npm install && npm run build

# Build the teem binary. Depends on `ui` because cmd/teem/ui_embed.go
# refuses to compile if cmd/teem/ui/dist is missing.
build: ui
	go build ./cmd/teem

# Remove SPA build artefacts. `go clean` handles the Go side.
clean:
	rm -rf cmd/teem/ui/dist cmd/teem/ui/node_modules

test: ui
	go test ./...

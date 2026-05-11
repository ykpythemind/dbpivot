// Package scenario contains end-to-end scenario tests that spin up real
// PostgreSQL instances via testcontainers-go and exercise the full
// dbpivot stack (proxy + control plane + CLI calls).
//
// These tests are gated behind the build tag `scenario` so they do not run
// during the default `go test ./...`. To run them:
//
//	go test -tags=scenario ./scenario/...
//
// Requires Docker (or a compatible runtime) to be available on the host.
package scenario

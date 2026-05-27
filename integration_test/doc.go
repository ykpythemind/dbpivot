// Package integration_test contains end-to-end integration tests that spin
// up real PostgreSQL and MySQL instances via testcontainers-go and exercise
// the full dbpivot stack (proxy + control plane + CLI calls).
//
// These tests are gated behind the build tag `integration_test` so they do
// not run during the default `go test ./...`. To run them:
//
//	go test -tags=integration_test ./integration_test/...
//
// Requires Docker (or a compatible runtime) to be available on the host.
package integration_test

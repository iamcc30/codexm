# Unintegrated Go proof artifacts

Go source snapshots in this directory use `.go.txt` extensions deliberately.
They are not additional packages in the repository's normal `go test ./...` or
production build. Isolated CI first tests/builds unchanged codexm, then copies
the snapshots into their intended package directories in the disposable runner,
formats those copies, and runs the focused tests, race detector, and vet.

`historycompat` contains read-only preflight checks with injected process and
configuration inspectors. It has no authoritative production configuration
adapter. General ChatGPT managed configuration remains unresolved; passing a
test stub or changing authentication modes is not an acceptable substitute.

`session` contains private durable-plan orchestration with a synthetic model
backend, not a native SQLite or native thread-metadata implementation. See its
status document for precise supported and unsupported operations. Neither proof
is connected to the shipped CLI, and neither establishes production acceptance.

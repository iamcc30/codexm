# Read-only native compatibility guards

Active implementation and tests now live in `internal/historycompat`. It validates
exact packaged executable hashes, Linux support, canonical file identities,
configuration layers and caller-supplied process/configuration evidence before
maintenance. The Linux adapter in `internal/session/native_history_linux.go`
supplies the restricted effective environment and stopped-process inspection.

A process scan is an observation, not a lock. Controlled launches also acquire the
shared profile lease; external native clients must remain stopped throughout the
operation. Other operating systems and unsupported configuration fail closed.

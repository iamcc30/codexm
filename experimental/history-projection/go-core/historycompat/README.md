# Read-only preflight prototype

This package is not connected to the CLI and is not a production compatibility guarantee. Check and Revalidate never execute native Codex, open databases, or write profile data. Synthetic tests provide scanner and configuration evidence adapters. Production adapters are not implemented: supplying a boolean or a constant fingerprint does not establish effective configuration.

Only Linux and exact packaged native/helper artifact digests targeting 0.166.0 are eligible. No passthrough arguments are accepted yet. All writers must be stopped and remain stopped; scanning is only an observation, not mutual exclusion against arbitrary processes. Caller operation leases are still required. Files and directory identities, hashes, absent configuration paths, and full launch inputs are frozen and rechecked. Changes after the final observation remain a race.

Upstream 0.166.0 source audit:

- config/src/loader/mod.rs: system /etc/codex/config.toml and requirements.toml; legacy /etc/codex/managed_config.toml; user config.toml and named profiles; cwd/config.toml and ancestor .codex/config.toml. This prototype rejects system/project layers and named profile selection rather than emulate trust or precedence.
- core/src/config/requirements.rs: apply_exact!(sqlite_home). config/src/config_requirements.rs: ConfigRequirementsToml supports sqlite_home. Exact managed requirements replace ordinary configuration, including CLI overrides.
- tui/src/lib.rs: the embedded interactive path installs managed_config_bundle_loader_for_storage, including after onboarding login. Generic ConfigBuilder defaults alone do not describe this path.
- cloud-config/src/bundle_loader.rs and service.rs: cloud configuration can come from authenticated service/cache and refresh during runtime. cloud-config/src/cache.rs stores cloud-config-bundle-cache.json in CODEX_HOME, or managed-config-{namespace}.json for other sources. Cache absence alone does not prove cloud policy absence.

Sources: https://github.com/openai/codex/tree/rust-v0.166.0/codex-rs

Before production integration: prove a concrete supported authentication/launch mode cannot introduce dynamic storage policy; implement complete process inspection; obtain trusted release artifact manifests; integrate native schema/transaction checks and recovery; run isolated Go and native acceptance suites. Do not silently replace these gates with caller assertions.

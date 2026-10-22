# Warpgate API compatibility

Compatibility covers the existing Target Group, MySQL / PostgreSQL / HTTP Target, role assignment, and synchronization deletion workflows. Updating this client does not add support for every Warpgate management API or new feature.

## Upstream contract review

The [0.29.0 release](https://github.com/warp-tech/warpgate/releases/tag/v0.29.0) explicitly announces breaking API changes. Comparing v0.28.2, v0.29.0, v0.29.2, and v0.30.0 identified the following changes relevant to this tool.

| Change | Current behavior |
| --- | --- |
| Starting with 0.29, Target writes require `ticket_requests_disabled`, `ticket_require_approval`, and `require_approval` | This fix explicitly sends `false` for new targets, preserves existing `true` / `false` values on updates, and uses defaults only for fields absent from older responses |
| 0.29 removes database `options.password` and requires `options.auth` | The existing implementation already sends `auth: {kind: Password, password: ...}`; legacy response fields are supported for reading |
| 0.29 HTTP `headers` no longer accepts `null` | The existing implementation normalizes empty headers to `{}` for new targets and distinguishes omitted fields from explicit clearing on updates |
| 0.29 PostgreSQL `protocol_version` is no longer optional | Writes always include an explicit `3.0` / `3.2` value. An empty inventory field preserves the existing target's version, falling back to Warpgate's default `3.2` for new targets or legacy responses without a version. Planning uses the same rules to avoid repeated updates |
| 0.29 changes session, MFA, and SSH host key APIs | This CLI does not call those APIs; these features are outside its validation scope |
| 0.30 passwords support Secret references, and responses hide credentials when permissions are insufficient | Ordinary passwords remain strings; full synchronization requires `TargetsEdit`, and Vault/OpenBao backend configuration is outside this tool's scope |
| 0.30 requires Ticket duration and usage limits to be at least 1 | Existing non-null values of 0 or less cause updates to be rejected; this tool preserves existing values instead of silently removing limits |

Sources: [0.29 TargetDataRequest](https://github.com/warp-tech/warpgate/blob/v0.29.0/warpgate-admin/src/api/targets.rs), [0.29 TargetOptions](https://github.com/warp-tech/warpgate/blob/v0.29.0/warpgate-common/src/config/target.rs), and [0.30 Target API](https://github.com/warp-tech/warpgate/blob/v0.30.0/warpgate-admin/src/api/targets.rs). The `targets.rs`, `target_groups.rs`, `roles.rs`, and `config/target.rs` files are identical in v0.29.0 and v0.29.2.

The client uses the following 11 HTTP operations. Their paths, success statuses, and response container types remain compatible across these versions. Lists remain JSON arrays and do not require paginated response decoding.

| Path (prefix: `/@warpgate/admin/api`) | Methods |
| --- | --- |
| `/target-groups` | GET, POST |
| `/target-groups/{id}` | PUT |
| `/targets` | GET, POST |
| `/targets/{id}` | PUT, DELETE |
| `/roles` | GET |
| `/targets/{id}/roles` | GET |
| `/targets/{id}/roles/{role_id}` | POST, DELETE |

Authentication continues to use `X-Warpgate-Token`. Local integration tests use the isolated instance's bootstrap admin token; production API token permissions and expiration require separate verification. Depending on the operation, synchronization requires permissions such as `TargetsCreate`, `TargetsEdit`, `TargetsDelete`, and `AccessRolesAssign`. Since 0.30 redacts responses without `TargetsEdit`, a plan made with a read-only token cannot establish that credential configuration matches.

## Reproducible verification

Local verification on 2026-10-22 used official Docker images with randomly generated credentials and synthetic placeholder data. It did not connect to production databases or real targets.

```shell
go test ./... -race -coverprofile=/tmp/warpgate-admin-compat.cover
go vet ./...

docker pull ghcr.io/warp-tech/warpgate:0.28.2
docker pull ghcr.io/warp-tech/warpgate:0.29.0
docker pull ghcr.io/warp-tech/warpgate:0.29.2
docker pull ghcr.io/warp-tech/warpgate:0.30.0

# Upgrade through each version using the same isolated SQLite database
python3 scripts/test-warpgate-compatibility.py

# Verify the planned direct upgrade path
python3 scripts/test-warpgate-compatibility.py --versions 0.28.2 0.30.0
```

The [test script](../scripts/test-warpgate-compatibility.py) builds the current CLI, publishes only random localhost ports, and removes its containers and volumes on exit. It does not read production configuration or accept an external Warpgate address. Any failed assertion produces a nonzero exit code.

Each version is tested for group creation and updates; creation, updates, and deletion of all three Target types; role assignment, default preservation, and explicit removal; preservation of approval policies, rate limits, and Ticket settings; PostgreSQL idle timeout preservation and protocol version updates; HTTP header and external host preservation and clearing; forced password updates; and unchanged repeated `plan` and `apply` runs. Upgrade checks compare existing Targets and roles field by field, allowing only known upstream changes: adding `require_approval` with a default of `false` and removing an empty legacy `options.password`. The actual `auth.password` value must remain intact.

Unit tests cover creation requests for all three protocols, all eight approval boolean combinations, and missing fields in legacy responses. Applying these regression tests to the original implementation fails; the original CLI also receives HTTP 400 when creating a Target on a real 0.29.0 instance. Integration tests confirm that newer versions reject writes missing any approval field and that 0.30 rejects nonpositive Ticket limits.

## Upgrade validation limits

- Verification covers management APIs and isolated SQLite migration. It does not cover real MySQL / PostgreSQL / HTTP target connectivity, production database migration, SSO, session recordings, or SSH host key import.
- This change does not add IAM Role authentication, SSH / RDP / VNC / Kubernetes Targets, session management, MFA, or Vault/OpenBao backend management. Existing password-based inventories retain their format.
- Before upgrading, check existing Targets' `ticket_max_duration_seconds` and `ticket_max_uses`: only `null` or positive values are valid. Resolve invalid legacy values according to the intended policy instead of automatically removing limits.
- Once credentials, networking, database backups, and images are ready in the China production environment, run `plan → apply → plan` against dedicated test targets, then verify existing approval policies, roles, and backend connections. Rolling back a database migration requires a backup restoration plan; downgrading the image alone is insufficient.

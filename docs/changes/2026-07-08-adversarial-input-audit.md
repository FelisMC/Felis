# Adversarial input-validation audit — every dangerous sink fails closed

- **Type:** negative-path / input-validation audit (no production code change)
- **Date:** 2026-07-08
- **Area:** `internal/api`, `internal/naming`, `internal/submit`, `internal/rcon`
- **Task:** a different question from the #82 / round-2 mutation audits. Those asked
  *do the TESTS catch a gate regression?* This asks *does the CODE reject hostile
  INPUT, or does bad data PASS?* — feed the real endpoints malformed, boundary, and
  hostile bodies (故意加错误数据) and confirm they fail closed (4xx) rather than letting
  the garbage reach a sink.

## Method — sink-first, not fuzz-everything

The low-hanging garbage (oversized body, unknown field, wrong content-type) is already
caught by the universal body guards, so a blanket "fuzz all ~80 handlers" would burn
effort where the answer is known. The real "can bad data PASS?" risk lives at the
**sinks** — the few places a request string is concatenated into an RCON command, used
as a K8s object name, joined into a filesystem/archive path, put in a SQL query, or
accepted as an enum/quantity **without a validator in front**. So the audit traces each
dangerous sink class from its handler entry to the sink, and for the crown-jewel class
(text → RCON) **mutation-verifies** the guard is non-vacuously pinned: loosen the guard
in source, run the package tests, confirm the specifically-named negative test reddens
(`--- FAIL: <subtest>`), then revert. Oracle: WSL Fedora-44, go1.26.4.

## The universal belt (caught before any sink)

`decodeJSON` (`internal/api/util.go`) wraps every body in
`http.MaxBytesReader(w, r.Body, 1<<20)` (1 MiB cap), sets `DisallowUnknownFields()`, and
rejects trailing data after the first JSON value — all → `400 bad_request`.
`requireJSONContentType` returns `415 unsupported_media_type` on credential writes
(a CSRF belt). So oversized, unknown-field, multi-document, and wrong-type bodies never
reach a handler body at all.

## The dangerous sinks — each traced fail-closed

### 1. Text → RCON (the lead). Two vectors, both fenced.

**(a) Structured commands** — `handlers_access.go` (LuckPerms permission/group,
whitelist/ban/kick). Every operand is validated against an anchored allow-list charset
*before* it is concatenated: `mcNameRe = ^[A-Za-z0-9_]{1,16}$` (player),
`lpNodeRe = ^[A-Za-z0-9_.*-]{1,64}$` (node), `lpCtxRe = ^[A-Za-z0-9_-]{1,48}$`
(world/group). No space, separator, or control character can appear in a validated
operand, and **there is no free-text field anywhere** — a ban/kick deliberately carries
no reason string (that would be the one splice vector). Go's `$` is `\z` (absolute end,
not `\Z`), so even a single trailing `\n` is rejected.
*Mutation-verified:* loosening `mcNameRe` to admit a space
(`^[A-Za-z0-9_ ]{1,16}$`) reddens
`TestAccessInjectionRejected/{whitelist,ban,kick,permission}_player_space` — the guard
is real, not vacuous.

**(b) Free-text passthrough** — `handlers_console.go` `handleCommand`, POST
`/servers/{name}/command`. This is the *one deliberate* free-text → RCON vector, and it
is **owner/admin-gated** (403 for a stranger, 404 for an unknown server). Its input
fence: trim + strip a single leading `/`, reject empty, cap at 1024 bytes, and
`strings.IndexFunc(command, func(c rune) bool { return c < 0x20 }) >= 0 → 400` — every
C0 control (incl. `\n`) is rejected so one request cannot splice a second command.
*Mutation-verified:* disabling the scan (`c < 0x20` → `c < 0x00`) reddens
`TestConsoleCommand/control_character_(newline)_->_400,_no_RCON_call`, whose input is
literally `{"command":"say hi\nop attacker"}` and whose assertion is 400 **and**
`console.calls == 0`. (Severity note: because this vector is owner-gated by design, the
scan is an audit-integrity measure — one request = one command — not a privilege
boundary; a splice on your *own* server escalates nothing, since the owner may already
run any RCON command. The fence exists regardless.)

### 2. Break-glass / internal-face (the newest code — scrutinised specifically)

This surface runs under a "service-token-authed / local-root, inputs trusted" posture,
the classic place a field-level guard gets skipped. Traced end to end:

- **Server name** — every internal handler (`handleReady`, `handleJoinEvent`,
  `handleInternalWake`, `handleInternalClaim`, `handleInternalMenuStatus`) validates the
  path name with `naming.ValidateServerName` before use.
- **`mc_uuid`** (join/wake/claim bodies) — checked non-empty, then flows *only* to
  DB-parameterized calls (`RecordJoin`, `UserByMCUUID`, `UUIDInAllowlist`). The
  "allowlist" is a **DB table**, not a live RCON `whitelist add` — there is no
  `mc_uuid` → RCON path.
- **Break-glass "OP-create"** (`performAddOperator` → `provisionOperator` →
  `InsertOperator(ctx, id, username, email)`) is a **parameterized DB INSERT** creating a
  *panel staff account*, **not** a Minecraft `op` RCON command. The hypothesised
  name → RCON `op` sink was checked and **does not exist** in this shape; the username is
  `TrimSpace`d and reaches only `$N`-parameterized SQL, from a local-root caller.
- **`os_user`** (internal backup attribution) — `TrimSpace`d, sets only the audit actor
  (a DB row); shown non-vacuous in the round-2 backup/restore audit (`b7b4a3b`).

### 3. SQL injection — dismissed.

`pgrepo.go` uses uniform `$1/$2/$3` parameterization throughout
(`QueryRowContext`/`ExecContext(ctx, q, args…)`); no request string is `Sprintf`'d into a
query.

### 4. Path traversal (submit) — dismissed.

`internal/submit` validates the submission id (rejects `..`, path separators, uppercase,
space, empty), and the on-disk blob name is a **fixed** constant (`contextBlobName`) — no
attacker-supplied filename is ever joined. The hostile-id matrix
`{"../evil","sub/../../etc","SUB-UPPER","has space","","a/b"}` is test-pinned in both the
local and S3 backends. The one free-form field a submission carries (`DisplayName`) is
charset-constrained by `displayNameRE` and rejects control chars
(`submit_test.go` "control chars" case).

### 5. K8s object names — validated at every cluster write.

`ValidateServerName` / `ValidateSystemServerName` (`^[a-z0-9-]{3,32}$`, no
leading/trailing dash, reserved-name set) and `ValidateHostname`
(`dnsLabelRE`, single label under the configured root domain) gate every create/patch.
`cluster.go` documents the invariant: "there is no free-form YAML path — every field is a
typed, validated value," so no raw CRD field can be smuggled through a create/patch body.

### 6. Numeric / enum — fail-closed.

`resolveResources` routes **every** quantity (memory, `resources.cpu/memory`, requests)
through `parsePositiveQuantity`, which rejects `q.Sign() <= 0` (negative *and* zero) with
a field-named 400, plus a request>limit guard; `parseStorageSize` carries the same guard.
Enums are closed sets: `parseAutostartPolicy` (ownerOnly/public/allowlist), the
access-action switch, and the image `ImageAdmitted` allow-list (no free image string).

## Verdict

**Bad data does not pass.** Every dangerous sink is fail-closed — including the
break-glass / internal-face surface, where the hypothesised `mc_uuid`/OP-create → RCON
paths were traced and found not to exist (parameterized DB, not RCON). Both text → RCON
vectors — structured (`handlers_access`) and free-text (`handleCommand`) — are
mutation-pinned by their named negative tests. No gap was found and no production code
changed; the honest result of "故意加错误数据" is that the input surface rejects it.

Scope is deliberately bounded to the dangerous sinks and the newest (break-glass) code,
not an exhaustive fuzz of all ~80 handlers — the claim proven is "every place user input
reaches a dangerous sink validates before the sink," by trace plus two mutations, not
"every handler was fuzzed."

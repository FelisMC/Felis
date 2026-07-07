# Test-quality integrity audit — do the verifications verify FUNCTION, or just go green?

- **Type:** audit / verification evidence (no code changed)
- **Date:** 2026-07-07
- **Method:** mutation testing on the WSL oracle (go1.26.4) + per-function coverage backbone
- **Scope:** the load-bearing safety invariants the backfilled change ledger *claims* were tested
- **Tree state:** every mutation reverted; authoritative Windows-git working tree clean at `5a7cd5a`
- **Point verified:** each gate is broken at `HEAD` (`5a7cd5a`), not per-commit — this is the
  right reading of "does the verification verify the FUNCTION": the current test pins the
  current implementation. A per-commit sweep would audit history hygiene, a different question.

## Why this audit exists

The ledger backfill asserts, per subsystem, that a set of load-bearing safety
properties are "unit-tested". A passing suite proves the tests are GREEN; it does
not prove they would go RED if the behaviour broke. Those are different claims —
"passing ≠ verifying". This audit closes that gap the only way that earns the word
*verified*: **break the implementation, confirm the specific test turns red.** A
subagent (or a human) *reading* a test and judging it "looks thorough" reproduces
the exact error being audited (looks-right ≠ verifies), so reading was used only to
locate the gate line; the verdict is always the mutation result.

## Result: 18 / 18 crown-jewel invariants mutation-verified

Each row is a one-line break of the implementation, run against its own package on
the oracle. **CAUGHT = the suite went red** = the test genuinely pins the behaviour.

| # | Invariant (claimed tested) | Impl gate mutated | Verdict |
|---|---|---|---|
| 1 | Pinned component is NEVER changed | `plan.go` pin branch → fall through | CAUGHT |
| 2 | A downgrade is NEVER proposed | `plan.go` `lv.After(current)` → `true` | CAUGHT |
| 3 | A prerelease is NEVER auto-applied | `plan.go` `!lv.IsPrerelease()` → `true` | CAUGHT |
| 4 | Apply ONLY inside the SysAdmin window | `plan.go` `Window.Contains(now)` → `true` | CAUGHT |
| 5 | `After` is strict (no equal-version churn) | `version.go` `> 0` → `>= 0` | CAUGHT |
| 6 | Clone-warned assertion refused fail-closed | `handlers_passkey.go` `if va.CloneWarning` → `if false` | CAUGHT |
| 7 | Approval CAS builds exactly once | `submit.go` `if !won` → `if false` | CAUGHT |
| 8 | An `everyone` base is not fail-open | `cfsetup.go` `if !includeHasEveryone` → `if true` | CAUGHT |
| 9 | Scoped-identity recognition actually admits | `cfsetup.go` `scoped = true` → `scoped = false` | CAUGHT |
| 10 | SSE per-principal stream cap holds | `api.go` cap-disable threshold | CAUGHT |
| 11 | OTP atomic reserve → one winner per burst | `api.go` `Sub(last) < window` → `< 0` | CAUGHT |
| 12 | Idle server is auto-stopped | `reconciler.go` `AutoStopEnabled &&` → `false &&` | CAUGHT |
| 13 | Startup/readiness timeout fires | `reconciler.go` `>= timeout` → `>= timeout + 1h` | CAUGHT |
| 14 | `/readyz` 503s when DB/K8s is down | `handlers_internal.go` dep-check `err != nil` → `false` | CAUGHT |
| 15 | NodePort fence only fires once connector serves | `tui_edge_apply.go` `connectorConnCount` parse-fail `return 0` → `1` | CAUGHT |
| 16 | CRITICAL-CVE build is NEVER admitted | `build.go` scan-gate `JobFailed`→`StatusFailed` → `StatusSucceeded` | CAUGHT |
| 17 | A user can NEVER claim a reserved system name | `naming.go` `reserved[name]` → `reserved["__nomatch__"]` | CAUGHT |
| 18 | Service token reaches ONLY the login pod | `builders.go` `Name == SystemLoginServer` → `true` | CAUGHT |

Rows 15–18 close the gap a review of this audit surfaced: the first pass verified a
*subset* and worded the verdict as the whole set. They are the four remaining
load-bearing safety properties the ledger docs name as "unit-tested" (§ *Documented-tested
claim reconciliation* below). Each mutation produced a real `--- FAIL` on the specifically
named test — e.g. #15 reddened `TestConnectorConnCount/garbage_is_not_a_healthy_tunnel`,
#16 `TestSyncFailedDoesNotAdmitImage`, #18 `TestBuildEnvWithholdsServiceTokenFromUserServers`
— i.e. an assertion failure, not a compile break.

Not one crown-jewel test was vacuous. The `cfsetup` fail-closed test additionally
feeds five distinct *violating* policies (bare-everyone, everyone-OR-identity,
unrecognized `ip` type, empty rule, wrong decision) and asserts each is rejected —
strong negative-path coverage, confirmed by mutations #8–#9.

## Coverage backbone — what no oracle test executes (failure-mode B)

Coverage triages code that no test even runs (so it cannot be verified). It does NOT
itself earn "verified" — high coverage with weak asserts is the same green-number
trap. Per-function scan of the security packages:

**Integration-only by design (0% on the oracle — honest, NOT a gap).** The real
adapters run only against live infra; unit tests exercise the ports through fakes:
- `pgrepo.go` — all SQL, **including `QuotaAvailable`/`QuotaCheck` (the quota TOCTOU
  atomic claim)**. This matches task #45's own "ENV-blocked" note: the atomic claim
  is a Postgres `INSERT … WHERE`, verifiable only against a real DB.
- `k8scluster.go`, the K8s console/log-stream adapters — real Kubernetes/RCON I/O.
- `tui_edge_apply.go` `verifyConnectorServing` + the nftables fence apply — shell out to
  live `cloudflared`/`nft`. **Correction from the first pass:** the doc splits this from the
  *pure* `connectorConnCount` decision gate, which IS unit-tested and is now mutation-proven
  (#15). The first pass wrongly folded the whole fence into "integration-only"; only the live
  calls are. The gate that decides *whether* to fence is verified.

**Genuine coverage gap (untested at the HTTP layer — "not verified").** These are
*missing* tests, not fake-passing ones:
- `handlers_users.go` — the P5 SysAdmin account-management suite: `handleCreateUser`,
  `handleGetQuotas`, `handleSetQuotas`, `handleListUsers`, `handleGetUser`,
  `handlePatchUser`, `handleDeleteUser`, `handleDisableUser`, `handleLinkAccount`,
  `handleUnlinkAccount`, `handleListUserSessions`, `handleRevokeUserSessions`, and
  `validateUsername`. All 0%; no `handlers_users*_test.go` exists. (The adjacent
  `DELETE …/passkeys` remediation handler *is* tested by `TestUnbindUserPasskeys`.)
- `handleReady` — the internal-face "server is up" push (distinct from the tested
  `handleReadyz`); 0%.

These handlers are owner/operator-role-gated, so the blast radius is bounded, but
`validateUsername` is load-bearing input validation and is the highest-value target
for a follow-up test. **Recommendation:** add an `handlers_users_test.go` covering
create/quota/link + `validateUsername` negative paths. Filed as a proposed change,
not made here (this is a read-and-verify audit — no test/impl was modified).

## Cheap tells (static pre-pass)

- 3 `t.Skip` sites, all benign: RNG-collision reruns (a 1-in-10^6 OTP code clash),
  not coverage-gating skips.
- No test file falls below 2 assertions per test function.

## Documented-tested claim reconciliation

To avoid the subset-verified/whole-worded trap a second time, every "unit-tested"
string in the ledger docs was enumerated (`grep -niE "unit-tested" docs/changes/*.md`)
and mapped to a verdict — verified fail-open gates get a mutation; behavioural/contract
claims are scoped, not silently dropped:

| Doc claim | Verdict |
|---|---|
| modpack: approval CAS builds once | mutation #7 |
| modpack: **scan in front of any push** | mutation #16 |
| cloudflare: `validateFailClosed` refuses public policy | mutations #8–#9 |
| cloudflare: **conn-count fence gate** | mutation #15 |
| system-servers: **naming reservation** | mutation #17 |
| system-servers: **service-token → login pod only** | mutation #18 |
| operator: idle stop / startup+readiness timeout / `/readyz` | mutations #12 / #13 / #14 |
| auto-update: pin / no-downgrade / no-prerelease / window / strict-`After` | mutations #1–#5 |
| passkey: clone-warned assertion refused | mutation #6 |

**Scoped, NOT individually mutation-proven** (behavioural/contract-level, not fail-open
safety gates — they rest on the green suite + the coverage backbone, and are called out here
rather than folded into the verdict):
- console-auth: content-type guard, anti-enumeration, forced-change lockdown. Anti-enumeration
  is the one with security weight; the current public login door is email-OTP/passkey, and its
  anti-enumeration behaviour is a candidate for a future mutation pass.
- break-glass setup: owner-auth match/non-match over the fake store.
- username-reclaim + auto-update JSON round-trip: in-memory repo contract / serialization.

## Toolchain honesty

Only Go runs on the oracle. The felis-limbo plugin (Java) is podman-verified against
a real Limbo jar (#65); the limbo/lobby images carry a build+boot check (#63). Those
completions were never a green-Go-tests claim and are not audited as if they were.

## Verdict

The commit history's verification claims are **accurate**: all 18 load-bearing *fail-open
safety gates* the ledger names as tested — spanning every subsystem, reconciled one-for-one
against the docs' "unit-tested" claims above — are mutation-proven to pin behaviour, not
merely to pass. No crown-jewel test was vacuous. The shortfalls are (a) integration seams
unrunnable on the oracle by design (honestly classified — including the live
`cloudflared`/`nft` fence-apply, whose *decision* gate is nonetheless verified); (b) one
untested cluster of admin user-management handlers — a missing test, not a false green; and
(c) a residue of behavioural/contract-level "unit-tested" claims (console anti-enumeration,
break-glass owner-auth, repo/JSON contracts) that rest on the green suite plus coverage and
are scoped above rather than individually mutation-proven — the honest boundary of this pass.

**Method note.** The first pass mutation-verified 14 gates but worded its verdict as "every"
invariant; a review caught that 4 documented safety gates (fence, scan, naming, service-token)
were named-as-tested yet unverified, and one (the fence) was mis-classified as integration-only.
Those four are now mutation-proven (#15–#18) and the classification corrected. The lesson is
the audit's own thesis turned on itself: *reading a scope and judging it complete* reproduces
the *looks-right ≠ verifies* error — only the enumerate-and-mutate reconciliation earns the word.

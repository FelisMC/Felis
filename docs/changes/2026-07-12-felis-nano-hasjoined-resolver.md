# Felis-nano — the federating `hasJoined` multiplexer (step 1: the Go resolver)

- **Type:** feature (new endpoint) — the verifiable "brain" of Felis-nano
- **Date:** 2026-07-12
- **Area:** `internal/api` (`handlers_hasjoined.go` + test), `internal/api/api.go`
  (route + `AuthSources` field), `docs/openapi.yaml`, `go.mod`
- **Task:** Felis-nano provides a MultiLogin-like capability — one Velocity proxy that
  accepts logins verified by **several** Yggdrasil auth servers at once (Mojang + N
  third-party roots), 正版优先 (Mojang-first). This change builds **step 1**: the Go
  `hasJoined` multiplexer that does the federating verification. It is the only part of
  the plan that produces immediate verifiable hard evidence (a unit-tested HTTP endpoint);
  the two delivery shells that point Velocity at it (a JVM `-Dmojang.sessionserver` flag,
  and a thin reflection-hook plugin for third-party servers) are later steps.

## What Velocity asks for, and what this answers

On a Minecraft login Velocity's authlib computes the `serverId` hash and issues
`GET /session/minecraft/hasJoined?username=<name>&serverId=<hash>[&ip=<ip>]` against
whatever URL its `mojang.sessionserver` system property names. A 200 with a game profile
means "verified"; a 204 means "not verified" and authlib rejects the login. Vanilla points
this at Mojang alone. Felis-nano points it **here**, and this endpoint fans the same query
out to the configured Yggdrasil roots **in priority order**, returning the first source
that validates. Each upstream Yggdrasil runs its own `serverId`-hash check — the
multiplexer only relays, it computes no hashes.

## The one non-negotiable transform — per-source UUID namespacing

A third-party Yggdrasil's UUIDs are **self-asserted**: nothing stops a malicious source
from answering with a *genuine Mojang player's* UUID. If that UUID were emitted as-is, the
third-party could impersonate any Mojang player with full UUID fidelity — and the reclaim/
blacklist layer could never catch it, because its whole invariant is "the genuine Mojang
player has a **different** UUID from any squatter." That invariant would simply be false.

So the resolver rewrites every non-identity source's profile into a per-source namespace
**before it leaves the resolver** — the single entry point every login crosses:

```
canonical = UUIDv3(felisAuthNS, tag + ":" + nativeID)      // third-party
canonical = the source's UUID verbatim                     // Mojang (Identity: true)
```

MD5 (UUIDv3) preimage resistance means no third-party can mint a value inside Mojang's
UUID space; the per-`tag` prefix means two sources can't collide onto one identity. Every
downstream key — `account_links`, `username_blacklist`, owner checks — then sees exactly
one canonical UUID per real identity, so the reclaim invariant is true **by construction**,
not by assumption.

## Fail-closed details that bite if wrong

- **Bar gate at the chokepoint.** The canonical UUID is checked against
  `Repo.IsUsernameBlacklisted` *before* the profile is returned, so a reclaimed squatter
  stays out even on a consumer that has no limbo plugin. Keyed on the **dashed** canonical
  (`.String()`) — the exact form `Repo.ReclaimUsername` stores. A DB error there fails
  closed (non-200 → authlib rejects), matching the existing `handleCheckBlacklist` pattern.
- **Emit undashed.** authlib's `GameProfile` expects the 32-hex undashed `id`
  (`hex.EncodeToString(u[:])`); the DB/reclaim/blacklist keys are dashed. The resolver
  **checks** on the dashed string and **emits** the undashed one. Mixing the two forms is a
  silent gate miss — pinned by the tests below.
- **`properties` relayed verbatim** (`[]json.RawMessage`) so a source's signed textures
  survive the multiplexer untouched.
- **Inert by default.** `AuthSources` is nil until `cmd/felis` wires configured sources,
  so the endpoint 204s every login until deliberately configured — it ships off.
- **Public internal-face route.** authlib sends no service token, so the route is mounted
  `Public: true` on the internal face (like `/healthz`); no third face is introduced. The
  OpenAPI parity test enforces `x-felis-face: [internal]` + `x-felis-tier: public`.

## Files

| File | Change |
|---|---|
| `internal/api/handlers_hasjoined.go` | **new** — `handleHasJoined` + `resolveHasJoined` + `AuthSource`/`sessionProfile` types + `felisAuthNS` |
| `internal/api/handlers_hasjoined_test.go` | **new** — `TestHasJoined`, 7 subtests over `httptest` fake Yggdrasil roots |
| `internal/api/api.go` | `AuthSources []AuthSource` field (nil = inert) + `GET /session/minecraft/hasJoined` `Public` internal route |
| `docs/openapi.yaml` | `/session/minecraft/hasJoined` path — `x-felis-face: [internal]`, `x-felis-tier: public`, `security: []` |
| `go.mod` | promote `github.com/google/uuid` indirect→direct (first direct importer) |

## Verification evidence

Oracle: WSL Fedora-44, go1.26.4. `go build ./...` → `BUILD-OK`. Full `internal/api`
package green (`ok felis.lolicon.best/internal/api`), `go vet ./internal/api/` clean. The
full package (not a `-run` filter) was run because this change edits two shared surfaces —
the `API` struct and the `internalAPIRoutes()` table — where a route that isn't under
`/api/v1/` is exactly what a route-table-driven invariant test would trip; nothing
reddened.

`TestHasJoined` — 7 subtests, all PASS:

1. `mojang identity passthrough` — Mojang UUID unchanged, `properties` relayed.
2. **`thirdparty UUID rewritten, never emitted as-is`** — the security invariant: an evil
   source returns real Notch's Mojang UUID; the resolver emits neither that UUID nor any
   Mojang-space value, but the deterministic `UUIDv3(felisAuthNS, "littleskin:"+id)`.
3. `mojang priority wins over thirdparty` — Mojang-first ordering.
4. `fallthrough to thirdparty when mojang 204s` — priority scan continues past a 204.
5. `no source validates -> 204`.
6. `barred canonical UUID -> 204` — the reused reclaim bar gate holds at the resolver.
7. `missing username -> 204` — no source touched on a malformed query.

`TestOpenAPIMatchesServedRoutes` PASS — the new route's served facets match its
`docs/openapi.yaml` entry in both directions.

## Deferred (not in this change)

- **Source configuration** (step 2): a `tag`/`type`/`url`/`priority` schema and
  `cmd/felis` wiring that populates `AuthSources`. Until then the endpoint is inert.
- **Delivery shells** (step 3): Shell 1 = the `-Dmojang.sessionserver` JVM flag on
  Felis-managed proxies; Shell 2 = the thin reflection-hook Velocity plugin for
  third-party servers, plus a Velocity verification runbook. The user has a real
  server to test Shell 2 against.
- **Name-match / textures-signature enforcement** — deliberately out of scope: identity
  is `tag:nativeID`, not the name, and textures are the cosmetic bucket relayed verbatim.

# Cloudflare Tunnel + Access edge (cfsetup) + NodePort fencing (ledger backfill)

- **Type:** feature + fix — retroactive ledger entry
- **Date:** 2026-06-27 – 2026-07-01
- **Area:** `internal/cfsetup` (pure core + integration runner), `cmd/felis` (TUI edge flow), edge nftables fence
- **Commits:**
  - `53a7664` feat(cfsetup): recommended Cloudflare Tunnel + Access edge (§14) — domain- and IdP-agnostic; the load-bearing `validateFailClosed` allowlist refuses any policy that could be public; fail-shut 404 catch-all; the raw game host is never proxied
  - `ba13839` feat(breakglass): optional Tunnel + Access setup in the sudo TUI, an independent peer of Owner provisioning
  - `a531f5e` fix(cfsetup): keep the connector install in the host apply layer only (drop the duplicate `StartConnector`)
  - `2810fe8` fix(cfsetup): repoint a stale DNS record when routing a tunnel hostname
  - `7d3be64` feat(cfsetup): start the tunnel connector as a setup step
  - `346ec68` refactor(deploy): rework the cloudflare-edge walkthrough — restructured the edge TUI flow and added a tested `cfsetup` integration-runner path (with TUI height-measure/root tests)
  - `e058a64` feat(edge): close the panel NodePort to the public after the tunnel is up — nftables at prerouting `raw` (-300), before kube-proxy's NodePort DNAT, gated on the connector actually serving; loopback accepted first so the connector origin hop is untouched
- **Tasks:** #37 (fence panel NodePort to public after tunnel)

## What it did

Stood up the optional one-click Zero-Trust edge: a Cloudflare Tunnel routing only the web
hostnames plus a fail-closed Access application, provisioned from the sudo TUI against the
operator's own Cloudflare account. `e058a64` then closes the Access-bypass hole where a
direct `https://<node-ip>:<nodeport>/` with the right Host header reached the origin
behind Access, by fencing the NodePort at the nftables raw hook so the packet is caught on
its original destination port — but only once the connector is confirmed serving, so
fencing never severs the only web path to a live origin.

## Why

Access is only a security boundary if the origin cannot be reached around it. The
fail-closed policy guard (`validateFailClosed`) and the NodePort fence are the two
load-bearing safety properties: a policy that could be public aborts the run with nothing
created, and a routable-but-unfenced NodePort would defeat the whole edge.

> **Backfill note.** Reconstructed 2026-07-07 from the commit history. The policy guard,
> ingress generation, request bodies, gating, and the nftables ruleset shape / conn-count
> gate are unit-tested; the live cloudflared/Cloudflare-API and `nft` calls are
> INTEGRATION-ONLY (need a real account). KNOWN-LIMITATION: the fence targets nftables;
> firewalld-native coordination is deferred. Not independently re-verified for this doc;
> current tree green at `9911b8c`.

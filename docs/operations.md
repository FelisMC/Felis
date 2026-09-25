# Felis Operations Guide

What a Felis host needs, how big it should be, how to take Felis off it again, and where
the disaster-recovery procedures live. Fault-finding is in
[troubleshooting.md](troubleshooting.md); this document refers to its sections as §N.

Evidence tags follow troubleshooting.md: **[VM-VERIFIED]** was run on a real host,
**[CI]** runs end to end on every push to main (`.github/workflows/e2e.yml`),
**[GO-TESTED]** / **[SH-TESTED]** is covered by `go test` or the shell tests under
`deploy/`, **[CODE-ONLY]** is what the code does and has not been run end to end.

## 1. Supported hosts

`deploy/bootstrap.sh` provisions a single node. It needs systemd, root, and one of the
package managers below; everything else (Docker, k3s, PostgreSQL, the JRE, cloudflared)
it installs.

| OS family | Package manager | Architectures | Status |
|---|---|---|---|
| CentOS Stream 9 (firewalld active, PostgreSQL 13) | dnf | aarch64 | **[VM-VERIFIED]** install, same-version rerun, upgrade, uninstall and reinstall |
| Ubuntu 24.04 LTS | apt | x86_64 | **[CI]** fresh install, same-commit rerun, and upgrade from the newest release to the pushed commit |
| RHEL / Rocky / Alma 9, Fedora | dnf | x86_64, aarch64 | [CODE-ONLY] same code path as CentOS Stream |
| Debian 12, other Ubuntu releases | apt | x86_64, aarch64 | [CODE-ONLY] |
| openSUSE Leap / Tumbleweed | zypper | x86_64, aarch64 | [CODE-ONLY] |
| Arch Linux | pacman | x86_64, aarch64 | [CODE-ONLY] |

Pinned component versions (a fresh install gets exactly these; an installed k3s or
cloudflared is left as it is, see §4):

| Component | Version | Where it is pinned |
|---|---|---|
| k3s | v1.36.4+k3s1 | `FELIS_K3S_VERSION` in `bootstrap.sh` |
| cloudflared | 2026.9.1 | `FELIS_CLOUDFLARED_VERSION`, sha256 per architecture |
| Temurin JRE (Velocity) | 25, patch build pinned | `FELIS_JRE_VERSION`, sha256 per architecture |
| Go (nano builds) | 1.26.8 | `GO_PINNED_VERSION`, sha256 per architecture |
| Minecraft / Limbo / Paper / Velocity / LuckPerms | `deploy/game-stack.lock` | §15b |
| PostgreSQL | the distribution's package | 13 and 18 are exercised by the `pgint` CI job |

32-bit hosts are not supported: there is no k3s, JRE or Go build the installer will fetch
for them.

Two things the host must keep for as long as the install lives:

- **Its address.** The install is bound to the IPv4 address it was made on (the
  database connection string, `pg_hba.conf`, the network policies, the panel
  certificate and the default nip.io domain all carry it). Give the host a static
  address or a DHCP reservation before installing; the installer warns when the address
  is a lease, and the watchdog reports `host-address` when the host loses it
  (troubleshooting §13c). The k3s node name is pinned at install time, so a hostname
  change is harmless.
- **A synchronized clock.** The installer turns NTP on (chrony where nothing else can)
  and the watchdog reports a clock that stays unsynchronized. Allow outbound UDP 123,
  or set `FELIS_MANAGE_TIME_SYNC=0` on a host whose clock is kept another way.

The installer also makes the system journal persistent (capped at
`FELIS_JOURNAL_MAX_USE`, default 1G; `FELIS_MANAGE_JOURNAL=0` skips it) and writes the
admin kubeconfig `/etc/rancher/k3s/k3s.yaml` root-only: run `sudo k3s kubectl`.

One node is the whole supported shape. A world volume is a ReadWriteOnce claim on the
node's local-path storage, so a game server's pod is pinned to the node that first
scheduled it and cannot move when that node fails; the operator and felis-api each run
as a single replica without leader election, so an upgrade or a node restart pauses
wakes and stops until their pod is back. Joining k3s agents to the cluster is untested
and gains no failover. A multi-node shape would need, at least, storage that can follow a
pod to another node and leader election in felis-operator (controller-runtime's
`LeaderElection`) so a second replica can stand by.

### While felis-api restarts

An installer rerun that changes felis-api, a node restart or a crashed pod takes the API
away until its new pod is ready: about 12 s on the reference VM (`kubectl rollout
restart` to Available). Its Deployment keeps one replica with the Recreate strategy, so
the old pod is gone before the new one starts. Two pods at once would be wrong for
felis-api: the uploads volume is ReadWriteOnce, a chunked upload is serialized inside the
process, and the build reconciler, restore settler, registry pruner, upload reapers and
audit retention run in-process without leader election, so each would run twice. During
the window:

- Players already on a server stay there; game servers keep running.
- A player leaving the login gate or joining a server by its address is admitted when
  felis-api confirmed their link within the last 10 minutes; anyone else is told login
  verification is temporarily unavailable.
- The login gate retries a new login for up to 60 s and tells the player it is retrying,
  so a restart shorter than that only delays the login.
- Wakes, stops, `/link` and the panel wait for the API.
- A Velocity restart in the window routes on
  `/opt/felis/velocity/plugins/felis-link/last-servers.json`, the last server list the
  API answered with, until a refresh succeeds (every 15 s).

## 2. Sizing

### What the platform itself uses

Measured on the verification host (4 vCPU, 5.5 GB RAM, 6 GB swap, CentOS Stream 9
aarch64) with the control plane, the login and lobby system servers and one idle Paper
server running **[VM-VERIFIED]**:

| Process | Resident memory |
|---|---|
| k3s (server, kubelet, containerd) | ~1.1 GB |
| Velocity (`-Xms512M -Xmx1G`, heap pre-touched) | ~0.73 GB |
| lobby (Paper, pod limit 1 GiB) | ~0.7–0.85 GB |
| login (Limbo, pod limit 512 MiB) | ~0.16 GB |
| felis-api, felis-operator, registry gate | ~50 MB each |
| PostgreSQL | ~30 MB plus page cache |
| **Total in use** | **~3.4 GB** |

Every game server adds the memory its owner gave it: the pod's limit equals its request,
and the JVM heap is derived from it (§1a). Quotas cap it per user (panel → 管理 → 配额).

The installer's own peak is the image builds (Docker plus a Gradle container); it stops
Docker afterwards so that memory goes back to the servers. On a host under 2 GB of RAM
without swap it adds a 2 GiB `/swapfile`.

### Recommendations

| Concurrent players | Game servers running | CPU | RAM | `FELIS_VELOCITY_XMX` |
|---|---|---|---|---|
| up to 20 | 1–2 small | 2 vCPU | 4 GB + 2 GB swap | 1G (default) |
| up to 100 | 3–5 | 4 vCPU | 8–16 GB | 1G |
| up to 300 | 5–10 | 8 vCPU | 16–32 GB | 2G |
| 300+ | more | 8+ vCPU | 32 GB+ | 3G–4G |

The player-count rows are planning figures, not measurements: a Minecraft server's cost
depends mostly on what its players do (view distance, redstone, mods). Size RAM as the
platform's ~3.5 GB plus the sum of the servers you expect to run at once, then add a
quarter for the page cache and PostgreSQL. Velocity itself needs little per player; raise
its heap when `journalctl -u felis-velocity` shows long GC pauses or `OutOfMemoryError`.

`FELIS_VELOCITY_XMX` (default `1G`, at least `256M`, written `<n>M` or `<n>G`) is read on
every installer run. The initial heap stays at 512M, or equals the maximum when that is
lower. Changing it rewrites the unit, and the rerun restarts the proxy, which disconnects
everyone online; do it in a quiet hour **[VM-VERIFIED]**:

```
curl -fsSL <raw-url>/deploy/bootstrap.sh | sudo FELIS_VELOCITY_XMX=2G bash
```

### Disk

| What | Where | Size |
|---|---|---|
| Worlds | one volume per server under `/var/lib/rancher/k3s/storage` | what the world grows to |
| World archives | the `felis-backups` volume (`FELIS_BACKUP_STORAGE`, default 10Gi requested) | about one compressed world per backup kept |
| In-cluster registry | the `registry` volume (default 10Gi requested) | 2–3 GB for the stock images; grows with custom builds, pruned daily (§9) |
| k3s's containerd images | `/var/lib/rancher/k3s/agent/containerd` | 6–9 GB |
| Docker's images and build cache | `/var/lib/containerd` (Docker's containerd store) | 5–10 GB after repeated upgrades |
| Toolchains and sources | `/opt/felis` | ~2.5 GB |
| Database bundles | `/var/lib/felis/db-backups` | a few MB each, 14 daily kept |

k3s's local-path volumes do not enforce the requested sizes (§9), so every volume shares
the root filesystem. Give the host at least **40 GB**, and 60 GB or more once worlds and
custom images accumulate. The watchdog mails the owners when a watched filesystem passes
its threshold, and §13b covers a full disk. `docker builder prune -af` (with Docker
started) reclaims the build cache when space is short; the next upgrade rebuilds it.

## 3. Uninstall

`deploy/uninstall.sh` takes off what the installer put on. It prints what it will remove
and asks before it starts (`--yes` skips the question) **[SH-TESTED]
[VM-VERIFIED]**:

```
curl -fsSL <raw-url>/deploy/uninstall.sh | sudo bash -s -- --yes     # keep the data
curl -fsSL <raw-url>/deploy/uninstall.sh | sudo bash -s -- --purge   # remove the data too
```

With a private repository, fetch it the way the README fetches `bootstrap.sh`.

Both modes remove the `felis-*` systemd units and `cloudflared-felis.service`, the
Velocity user, `/opt/felis`, `/usr/local/bin/felis`, the installer's cloudflared binary
(unless another unit runs it), the `felis_postgres` and `felis_edge` nftables tables and
the firewalld ports the installer opened. k3s goes with k3s's own `k3s-uninstall.sh` when
the cluster holds nothing but Felis's namespaces; when it runs anything else only
`felis`, `minecraft`, `felis-build` and the MinecraftServer CRD are deleted.
`--keep-k3s` and `--remove-k3s` override that choice.

| | keep data (default) | `--purge` |
|---|---|---|
| Final database bundle | taken first (`felis db backup -label manual`); a failure stops the uninstall before anything is removed. `--no-backup` skips it | none |
| `felis` database and role | kept | dropped; `listen_addresses` and `pg_hba.conf` go back to how they were |
| `/etc/felis` (secrets, `felis.toml`, `offsite.env`, tunnel config) | kept; `bootstrap.done` and the per-run records go | deleted, with the tunnel's credentials file |
| `/var/lib/felis` (database bundles) | kept | deleted |
| Worlds, archives, registry, uploads | moved to `/var/lib/felis/retained/k3s-storage-<stamp>/` (with `--keep-k3s`: their volumes switch to `Retain` and stay in place) | deleted |
| Felis images, Docker build cache | kept | deleted |

Neither mode removes packages (Docker, PostgreSQL, git, nftables) or the swap file: other
software may use them. On a host that should end up bare:

```
sudo swapoff /swapfile && sudo rm /swapfile && sudo sed -i '\|^/swapfile |d' /etc/fstab
sudo dnf remove docker-ce docker-ce-cli containerd.io postgresql-server   # or apt/zypper/pacman
```

The Cloudflare side outlives the host. After an uninstall that is final, delete the
tunnel (Zero Trust → Networks → Tunnels, or `cloudflared tunnel delete <name>`), its
DNS records for the panel hostnames, and the Access application.

### Reinstall on top of kept data

A keep-data uninstall leaves everything a reinstall needs. The installer reuses
`/etc/felis/secrets.env`, so the database password and the forwarding and session
secrets are unchanged, and it migrates the kept database instead of creating one
**[VM-VERIFIED]**.

Each step below was run on the reference VM after a keep-data uninstall, and the
restored worlds matched their kept `level.dat` checksums **[VM-VERIFIED]**. `kept` names
the directory the uninstall moved the volumes to:

```
kept="$(ls -d /var/lib/felis/retained/k3s-storage-* | tail -n 1)"
store=/var/lib/rancher/k3s/storage
```

1. Install as usual (`curl ... | sudo bash`). Name the same root domain if it was not
   the `<ip>.nip.io` default: `felis.host.toml` is kept, and the installer reads the
   domain from it.
2. Run `sudo felis setup`. It recreates the login and lobby servers; the Owner already
   exists, so it opens on the status screen and you can quit there.
3. Put the image registry and the uploads back. They hold every custom server image
   and uploaded file; without the registry, a restored server fails to pull its image.

   ```
   sudo k3s kubectl -n felis scale deploy/registry deploy/felis-api --replicas=0
   sudo k3s kubectl -n felis wait --for=delete pod -l app.kubernetes.io/component=registry --timeout=120s
   sudo k3s kubectl -n felis wait --for=delete pod -l app.kubernetes.io/component=api --timeout=120s
   sudo rsync -a --delete "$kept"/pvc-*_felis_registry/ "$(ls -d $store/pvc-*_felis_registry)"/
   sudo rsync -a --delete "$kept"/pvc-*_felis_felis-uploads/ "$(ls -d $store/pvc-*_felis_felis-uploads)"/
   sudo k3s kubectl -n felis scale deploy/registry deploy/felis-api --replicas=1
   ```

   Then run the installer once more. It pushes this release's images over the older
   copies the kept registry carried.
4. Bring the game servers back. The final bundle holds every MinecraftServer as it was;
   the selector skips login and lobby, which step 2 created for this release:

   ```
   b="$(ls -t /var/lib/felis/db-backups/felis-db-*-manual.tar | head -n 1)"
   tar -xOf "$b" k8s/minecraftservers.json \
     | sudo k3s kubectl apply -l '!felis.lolicon.best/system-role' -f -
   ```

5. Put each world back. A server's volume exists once it has started once, so start it
   from the panel, stop it again, and copy the kept world over the new one:

   ```
   s=<server>
   sudo rsync -a --delete "$kept"/pvc-*_minecraft_world-$s-0/ "$(ls -d $store/pvc-*_minecraft_world-$s-0)"/
   ```

   Then start it. The lobby works the same way: stop it with
   `sudo k3s kubectl -n minecraft patch minecraftserver lobby --type=merge -p '{"spec":{"desiredState":"Stopped"}}'`,
   copy `world-lobby-0`, and patch it back to `Running`.
6. Bring the archives back so the panel's restore points work again. The archive volume
   appears with the first backup, so back up any server from the panel first, then:

   ```
   sudo rsync -a "$kept"/pvc-*_minecraft_felis-backups/ "$(ls -d $store/pvc-*_minecraft_felis-backups)"/
   ```

   With an off-site bucket configured, `sudo felis offsite fetch-worlds` fetches them
   instead (troubleshooting §16).
7. Delete `/var/lib/felis/retained/` once every server is back.

## 4. Upgrading the pieces around Felis

A rerun of the installer upgrades Felis itself (§15). The components it installs keep
the version they were installed with unless noted:

| Component | How a rerun treats it | Upgrade |
|---|---|---|
| Velocity, Limbo, Paper, LuckPerms | follow `deploy/game-stack.lock` | rerun after a release that moves the lock (§15b) |
| Temurin JRE | moves to the pinned patch build | rerun |
| k3s | left alone | rerun with `FELIS_UPGRADE_DEPS=1`: moves to the pinned release through that tag's install script, one minor version at a time (a bigger jump stops before anything changes and names the release to go through), never backwards |
| cloudflared | left alone | rerun with `FELIS_UPGRADE_DEPS=1`: swaps `/usr/local/bin/cloudflared` for the pinned, sha256-checked release and restarts `cloudflared-felis`; a cloudflared the distribution installed stays with its package manager |
| PostgreSQL | the distribution's package | the package manager for a minor release; a major version needs `pg_upgrade` first (below) |
| Docker, git, nftables | distribution packages | the package manager |

```sh
curl -fsSL https://raw.githubusercontent.com/FelisMC/Felis/main/deploy/bootstrap.sh \
  | sudo FELIS_UPGRADE_DEPS=1 bash
```

`sudo felis update` reports Felis, Velocity, k3s, cloudflared, the JRE and PostgreSQL
against their newest releases; `--k3s`, `--cloudflared`, `--jre` and `--postgres` narrow
it to one. PostgreSQL is compared within its major, since a minor release is a package
update, and a major past its end of life gets a note naming the current one.

The installer also sets up `felis-update-check.timer`, which runs `felis update --record`
once a day around 05:30 (and at boot after a missed run). `--record` stores the result
in `platform_settings`, and the panel's **Admin → Updates → Component versions** card
shows it: each component's installed and newest version, and for the ones with a newer
release the `sudo felis update --<component>` line that prints how to apply it. Felis
applies nothing on its own; the installer re-run above is the apply path. The card turns
red when the newest record is older than 26 hours, meaning the timer stopped:

```sh
systemctl list-timers felis-update-check.timer
journalctl -u felis-update-check -n 50 --no-pager
sudo felis update --record   # record a fresh check now
```

### PostgreSQL major versions [CODE-ONLY]

The installer takes the major the distribution ships (13 on EL9) and never moves it. To
go to a newer one, stop the writers, keep a dump, then use the distribution's upgrade
path:

```sh
sudo k3s kubectl -n felis scale deploy/felis-api deploy/felis-operator --replicas=0
sudo -u postgres pg_dumpall > /root/felis-pg-$(date +%F).sql
# EL9: sudo systemctl stop postgresql; sudo dnf module switch-to postgresql:16
#      sudo dnf install postgresql-upgrade; sudo postgresql-setup --upgrade
# Debian/Ubuntu: sudo pg_upgradecluster <old-major> main
sudo systemctl start postgresql
sudo k3s kubectl -n felis scale deploy/felis-api deploy/felis-operator --replicas=1
```

### The MinecraftServer CRD [VM-VERIFIED]

Every rerun applies the CRD embedded in the `felis` binary (`felis bootstrap-assets crd`).
It serves and stores the single version `v1alpha1`, and the apiserver refuses values the
operator cannot act on:

| Field | Accepted |
|---|---|
| `spec.rcon.port` | unset, `0` or `25575`: the allow-rcon NetworkPolicy opens only 25575, so any other port leaves the server unprobeable |
| `spec.startup.timeoutSeconds`, `readinessTimeoutSeconds` | 0 – 86400 |
| `spec.startup.healthHTTPPort` | 0 – 65535 |
| `spec.lifecycle.terminationGracePeriodSeconds` | 0 – 3600 |
| `spec.idle.emptySecondsBeforeStop` | 0 – 604800 (the panel caps it at 86400) |

`0` means the operator's default throughout. An object stored before these rules keeps an
out-of-range value until someone edits that field (CRD validation ratcheting). The operator
reads a negative value as its default and an oversized one as written, so fix such a
value by hand: `kubectl -n minecraft edit minecraftserver <name>`.

**Moving to `v1beta1` (planned, not built).** The first breaking change to the spec ships as a new
version, in this order, each step one release:

1. The CRD serves `v1alpha1` and `v1beta1`, storage stays `v1alpha1`. While the two
   schemas carry the same fields, `conversion.strategy: None` suffices; a renamed or
   reshaped field needs a conversion webhook, which felis-operator would serve.
2. Storage moves to `v1beta1`. The installer rewrites every object so etcd holds the new
   version (`kubectl get minecraftservers -A -o json | kubectl replace -f -`), then sets
   `status.storedVersions` of the CRD to `["v1beta1"]`.
3. A later release stops serving `v1alpha1`. Felis itself reads through one Go type at a
   time, so the operator and felis-api switch in the release that moves storage.

## 5. Disaster recovery

The procedures are in §16: what a database bundle holds, restoring one on the same host,
rolling back an upgrade, and rebuilding on a new host from the off-site copy. For a
production install:

- **Configure the off-site copy** (`FELIS_OFFSITE_*`, §16 "Keep a copy somewhere
  else"). Without it the world archives sit on the same disk as the worlds, and the
  database bundles on the same disk as the database; losing the disk loses both. The
  installer ends with `NO OFF-SITE COPY` until it is set.
- **Keep the off-site encryption key off the host**, in a password manager. The bucket
  holds only sealed objects.
- **Keep one database bundle off the host** as well when there is no bucket. It contains
  `secrets.env`, which a rebuild needs to read the rest.
- **Rehearse the rebuild** once on a spare VM: §16 "Rebuild on a new host", steps 1–5,
  then log in and restore one world. `felis offsite status` and `felis db check` exit
  non-zero when the copy or the newest bundle is stale; wire them into your monitoring,
  or rely on the watchdog's mail.

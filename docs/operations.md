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
package managers below; everything else (k3s, the JRE, cloudflared, and Docker when an image
has to be built on the host; see "Where the binary and the images come from" below) it
installs.
PostgreSQL runs inside k3s as the `felis-postgres` Deployment, from the official image the
release pins by digest, with its data on the host in `/var/lib/felis/postgres`.

| OS family | Package manager | Architectures | Status |
|---|---|---|---|
| CentOS Stream 9 (firewalld active, SELinux enforcing) | dnf | aarch64 | **[VM-VERIFIED]** fresh install from release assets and its rerun, upgrade from v0.1.0 (moving the database off the host PostgreSQL 13 into felis-postgres), uninstall and reinstall |
| Ubuntu 24.04 LTS | apt | x86_64 | **[CI]** fresh install and same-commit rerun from the pushed commit's release assets, and upgrade from the newest release onto them; the on-host build weekly |
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
| PostgreSQL | 18.6, the official `postgres` image by digest | `POSTGRES_IMAGE` in `bootstrap.sh`, `defaultPostgresImage` in `internal/platform` |

32-bit hosts are not supported: there is no k3s, JRE or Go build the installer will fetch
for them.

Before it changes anything the installer checks the host and reports every problem at
once, then stops with nothing touched **[SH-TESTED]**:

- the architecture, systemd as init, and the memory cgroup controller k3s needs;
- RAM: under 1.75 GiB is refused (a "2 GB" VPS passes), under 3.5 GiB is a warning;
- free disk on each filesystem it writes to, summed when they share one: on a bare host
  about 17 GiB installing a release, 15 GiB from `FELIS_ARTIFACT_DIR` and 23 GiB when it
  builds the images itself; 7 GiB for a rerun; a directory that already holds data
  (Docker's cache, a reused k3s) counting at the rerun size; a filesystem that would end
  over 85%, where k3s starts deleting cached images, is a warning;
- the ports it will listen on: the game port, the panel NodePort, k3s's 6443/6444 and
  10248–10259 and the registry's loopback 5000. A port held by the installer's own
  proxy or k3s is a rerun and passes;
- another Kubernetes (kubelet, RKE2, k0s, MicroK8s) or a k3s agent on the host;
- the node address or a routed network inside k3s's `10.42.0.0/16` and `10.43.0.0/16`
  (a Docker network there is the usual case); a wider route such as a `10.0.0.0/8` VPN
  is a warning;
- HTTPS to the hosts it downloads from: GitHub and PaperMC's download API always, Docker
  Hub when it builds images on the host. A host counts as reachable once a TLS handshake
  with it completes, and each gets three tries two seconds apart. Installing a release, an
  unreachable Docker Hub is a warning (it is needed only if an asset turns out unusable);
  from `FELIS_ARTIFACT_DIR` it is not checked.

`FELIS_PREFLIGHT=warn` reports the same problems as warnings and installs anyway, for a
host the checks misjudge.

Two things the host must keep for as long as the install lives:

- **Its address.** The install is bound to the IPv4 address it was made on (the k3s
  node, the network policies, the panel certificate and the default nip.io domain all
  carry it). Give the host a static address or a DHCP reservation before installing;
  the installer warns when the address is a lease, and the watchdog reports
  `host-address` when the host loses it (troubleshooting §13c). The k3s node name is
  pinned at install time, so a hostname change is harmless.
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

### Where the binary and the images come from

A release install (the default channel, and the setup console) takes everything Felis
builds from that release's assets, each checked against the release's `SHA256SUMS` before
it is used: the `felis` binary, the control-plane image, the limbo, lobby and paper images,
the registry and PostgreSQL images (at the digests `bootstrap.sh` pins), and
`felis-velocity.jar`. The images go into k3s's containerd with `k3s ctr images import` and
from there into the in-cluster registry, so the host needs no Docker, Gradle, Go or Docker
Hub for them. k3s's own images come from k3s's GitHub release
(`k3s-airgap-images-<arch>.tar.zst`, checked against k3s's sha256 list) before k3s first
starts. An upgrade downloads only the image tars holding an image the host lacks; they wait
in `/var/lib/felis/artifacts` until the registry has the images, and are deleted then.
`deploy/build-release-artifacts.sh` documents every asset. The decisions are **[SH-TESTED]**.
Installing from the assets is **[VM-VERIFIED]** on CentOS Stream 9 aarch64 through
`FELIS_ARTIFACT_DIR`: a fresh install and an upgrade over a release that built on the host
pulled no image and built nothing, and a rerun imported and uploaded nothing. Downloading them from a
release is [SH-TESTED] until a release publishes assets.

The installer builds on the host instead, installing Docker for it and stopping Docker once
the images are in the registry, when:

- the source is not a release: `FELIS_VERSION_BOOTSTRAP=dev`, a pinned `FELIS_REF`, or
  `FELIS_SKIP_FETCH`;
- `FELIS_GAME_STACK=latest`, for the login, lobby and paper images (the rest still come
  from the release);
- the release publishes no `SHA256SUMS` (one cut before release assets existed, or still
  uploading), or an asset is missing, fails its checksum or is malformed. Only that image is
  built (the registry and PostgreSQL images are pulled from Docker Hub instead), and a
  warning names it; troubleshooting §15c lists the messages.

`FELIS_ARTIFACT_DIR=<absolute path>` installs from a directory instead of the release: a
release's assets downloaded there (every `felis-*` file and `SHA256SUMS`), or the directory
`deploy/build-release-artifacts.sh <version> <dir>` wrote. Nothing of Felis's own is
downloaded or built (except the game images under `FELIS_GAME_STACK=latest`, which no release
ships), so an asset the directory lacks, or one failing its checksum, stops the install; k3s,
the JRE, cloudflared and Velocity still come from GitHub and PaperMC. It
cannot be combined with `FELIS_REF` or `FELIS_SKIP_FETCH`, which name a source too.

```
# on a machine with access: the release's assets for the host's architecture
gh release download v1.4.0 --repo FelisMC/Felis --dir felis-v1.4.0 \
  --pattern 'felis-*linux-amd64*' --pattern felis-velocity.jar --pattern SHA256SUMS
# on the host, after copying the directory over
sudo FELIS_ARTIFACT_DIR=/root/felis-v1.4.0 bash bootstrap.sh
```

`SHA256SUMS` lists both architectures; the files of the other one may be left out.

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

A longer outage reads like this in the logs [VM-VERIFIED]. The drill scaled felis-api
to 0 for about 8 minutes on the reference VM.

- The proxy logged `server list refresh failed ... keeping current registrations` 11 s
  in, then `still failing: 22 failed attempts over 304 s` at the 5-minute mark.
- The watchdog found `deployment/felis-api` critical on its first run after the scale.
  It raised the alert on the first run past 5 minutes, at about 7 minutes; with no
  `[smtp]` that is logged only (`journalctl -u felis-watchdog`).
- The proxy logged `server list refresh recovered after 32 failed attempts over 469 s`
  as soon as the new pod was Available.

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
| PostgreSQL (the felis-postgres pod) | ~30 MB plus page cache |
| **Total in use** | **~3.4 GB** |

Every game server adds the memory its owner gave it: the pod's limit equals its request,
and the JVM heap is derived from it (§1a). Quotas cap it per user (panel → 管理 → 配额).

A release install builds nothing (§1). When the installer builds on the host its peak is
the image builds (Docker plus a Gradle container); it stops Docker afterwards so that memory
goes back to the servers. On a host under 2 GB of RAM without swap it adds a 2 GiB
`/swapfile`.

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
| Docker's images and build cache | `/var/lib/containerd` (Docker's containerd store), on a host that built its images (§1) | 5–10 GB after repeated upgrades |
| Release assets during an install | `/var/lib/felis/artifacts` | up to ~2 GB, deleted once the images are in the registry |
| Toolchains and sources | `/opt/felis` | ~2.5 GB |
| Database | `/var/lib/felis/postgres` (felis-postgres's cluster) | tens of MB; the audit log is most of it |
| Database bundles | `/var/lib/felis/db-backups` | a few MB each, 14 daily kept |

k3s's local-path volumes do not enforce the requested sizes (§9), so every volume shares
the root filesystem. Give the host at least **40 GB**, and 60 GB or more once worlds and
custom images accumulate. The watchdog mails the owners when a watched filesystem passes
its threshold, and §13b covers a full disk. On a host that built its images,
`docker builder prune -af` (with Docker started) reclaims the build cache when space is
short; the next upgrade rebuilds it.

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
Velocity user, `/opt/felis`, `/usr/local/bin/felis`, the release assets an interrupted
install left in `/var/lib/felis/artifacts`, the installer's cloudflared binary (unless
another unit runs it), the `felis_edge` nftables table (and `felis_postgres`, which
releases before the database moved into k3s loaded) and the firewalld ports the installer
opened. k3s goes with k3s's own `k3s-uninstall.sh` when the cluster holds nothing but
Felis's namespaces; when it runs anything else only `felis`, `minecraft`, `felis-build`
and the MinecraftServer CRD are deleted.
`--keep-k3s` and `--remove-k3s` override that choice.

| | keep data (default) | `--purge` |
|---|---|---|
| Final database bundle | taken first (`felis db backup -label manual`); a failure stops the uninstall before anything is removed. `--no-backup` skips it | none |
| The database (`/var/lib/felis/postgres`) | kept; felis-postgres is stopped cleanly before k3s goes | deleted with `/var/lib/felis` |
| A host PostgreSQL an earlier release ran the database on | kept as it is: stopped after the move into k3s (below, §4), with its old copy of `felis` | its `felis` database and role are dropped (the server is started for that and stopped again), and `listen_addresses` and `pg_hba.conf` go back to how they were. Checked before anything is removed: a role that still owns another database (the `felis_pgint` the PG contract tests use, CONTRIBUTING.md) or holds grants elsewhere stops the purge up front with the list and the `ALTER DATABASE … OWNER TO postgres` to run |
| `/etc/felis` (secrets, `felis.toml`, `offsite.env`, tunnel config) | kept; `bootstrap.done` and the per-run records go | deleted, with the tunnel's credentials file |
| `/var/lib/felis` (the database, its bundles) | kept | deleted |
| Worlds, archives, registry, uploads | moved to `/var/lib/felis/retained/k3s-storage-<stamp>/` (with `--keep-k3s`: their volumes switch to `Retain` and stay in place) | deleted |
| Felis images, Docker build cache | kept | deleted |

The two database rows are [SH-TESTED] (`deploy/uninstall_test.sh`); the VM runs above
predate felis-postgres.

Neither mode removes packages (Docker, git, nftables, and the PostgreSQL server an earlier
release installed) or the swap file: other software may use them. On a host that should
end up bare:

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
secrets are unchanged, and the installer migrates the kept database instead of creating
one **[VM-VERIFIED]** (with the host database of the releases before felis-postgres).
felis-postgres starts again on the cluster kept in `/var/lib/felis/postgres` [SH-TESTED].

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
| PostgreSQL | follows the image the release pins | a minor release comes with a Felis release, and the rerun restarts felis-postgres on it (a few seconds without the API); a major version is a dump and restore (below) |
| Docker, git, nftables | distribution packages | the package manager |

```sh
curl -fsSL https://raw.githubusercontent.com/FelisMC/Felis/main/deploy/bootstrap.sh \
  | sudo FELIS_UPGRADE_DEPS=1 bash
```

`sudo felis update` reports Felis, Velocity, k3s, cloudflared, the JRE and PostgreSQL
against their newest releases; `--k3s`, `--cloudflared`, `--jre` and `--postgres` narrow
it to one. PostgreSQL is read from the felis-postgres container and compared within its
major, since a minor release arrives with a Felis release, and a major past its end of life
gets a note naming the current one.

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

### Bringing an older install up to date [VM-VERIFIED]

Three pieces of an install keep the shape they had on the day they were created, and
neither `felis setup` nor `kubectl rollout restart` reaches them: the felis-api
Deployment (an env var added later, such as `FELIS_SMTP_PASSWORD`, is absent until the
Deployment is rendered again), the lobby image (built with whatever plugins the recipe
had then; LuckPerms came later, and without it every permission change from the panel
answers `luckperms_missing`), and the `MinecraftServer` specs (a field added later stays
unset). Bring all three forward in this order, images first:

```sh
# 1. Rerun the installer: renders and applies the control-plane bundle, rebuilds and
#    re-imports the login and lobby images, and recreates those two pods so they run
#    the new images. The [smtp] relay the setup wizard wrote is carried forward.
curl -fsSL https://raw.githubusercontent.com/FelisMC/Felis/main/deploy/bootstrap.sh | sudo bash

# 2. Fill the spec fields the system servers gained since (troubleshooting §12b), then
#    RCON for user servers created before it was the default. -user-rcon waits on
#    each server's image opening RCON; see §12b before running it.
sudo felis converge
sudo felis converge -user-rcon
```

Check each piece:

```sh
kubectl -n felis get deploy felis-api \
  -o jsonpath='{.spec.template.spec.containers[0].env[*].name}' | tr ' ' '\n' | grep SMTP
kubectl -n minecraft exec lobby-0 -- ls /data/plugins | grep -i luckperms
kubectl -n minecraft get minecraftserver \
  -o custom-columns=NAME:.metadata.name,RCON:.spec.rcon.enabled,IDLE:.spec.idle.autoStopEnabled
```

The env var only carries the password; mail still needs the relay itself, set in
`felis setup` → email. A user server picks up its new RCON block at its next start.

### PostgreSQL major versions [CODE-ONLY]

felis-postgres keeps its cluster in `/var/lib/felis/postgres/<major>/docker`. A release that
moves the image to a new major finds the old major's cluster there and stops before it
changes anything: the new server would start an empty cluster beside it. The way across is
a bundle, taken on the release you run now, restored into the new major's empty cluster:

```sh
# On the release you run now:
b="$(sudo felis db backup -label pre-upgrade | sed -n 's/^felis db backup: wrote //p')"
sudo k3s kubectl -n felis scale deploy/felis-postgres --replicas=0
sudo mv /var/lib/felis/postgres/18 /var/lib/felis/postgres-18.old   # the old major's cluster, for a way back

# Install the new release: it starts an empty cluster on the new major and creates the schema.
curl -fsSL <raw-url>/deploy/bootstrap.sh | sudo bash

# Put the data back and bring its schema up to the new release.
sudo k3s kubectl -n felis scale deploy/felis-api deploy/felis-operator --replicas=0
sudo felis db restore -yes -no-safety-backup "$b"
sudo felis migrate up -config /etc/felis/felis.host.toml
sudo k3s kubectl -n felis scale deploy/felis-api deploy/felis-operator --replicas=1
```

Delete `/var/lib/felis/postgres-18.old` once the new release has run for a while. To go back
instead, scale felis-postgres to 0, move the new major's directory out of
`/var/lib/felis/postgres`, move `postgres-18.old` back as `/var/lib/felis/postgres/18`, and
rerun the older release's installer.

### The database's move into k3s [VM-VERIFIED]

Releases before the move ran the database on a PostgreSQL the installer installed on the
host. The first rerun of a release with felis-postgres moves it, once:

1. It stops felis-api, felis-operator and the host timers, and heads the host's
   `pg_hba.conf` with a block that refuses every connection to `felis` but its own copy
   (the original is kept beside it as `pg_hba.conf.pre-pg-move`).
2. It takes a `pre-pg-move` bundle of the host database (`felis db backup`), restores it
   into felis-postgres (`felis db restore`) and compares the row count of every table on
   both servers. Any failure up to here puts `pg_hba.conf` and the control plane back and
   the platform keeps running on the host database, untouched.
3. It stops and disables the host `postgresql` service, which stays installed with its
   copy of the data, and writes `/var/lib/felis/postgres-moved`. A host server that also
   holds other databases keeps running; its `felis` copy is then reachable over loopback
   only.

From then on the host config points at felis-postgres (`127.0.0.1:15432`, and
`deployment = "felis/felis-postgres"`, through which `felis db` runs `pg_dump`, `psql`
and `pg_restore` inside the pod) and the pods at `felis-postgres.felis.svc:5432`.

To go back to the host database, for instance to reinstall the release before the move:

```sh
sudo k3s kubectl -n felis scale deploy/felis-api deploy/felis-operator deploy/felis-postgres --replicas=0
hba="$(sudo -u postgres psql -XtAc 'SHOW hba_file' 2>/dev/null || echo /var/lib/pgsql/data/pg_hba.conf)"
sudo cp -p "${hba}.pre-pg-move" "$hba"
sudo systemctl enable --now postgresql
sudo rm /var/lib/felis/postgres-moved
curl -fsSL <raw-url-of-that-release>/deploy/bootstrap.sh | sudo bash
```

`SHOW hba_file` needs the server running; with it stopped, the fallback path is EL's
(Debian and Ubuntu keep it in `/etc/postgresql/<major>/main/`). Whatever the platform wrote
after the move lives only in felis-postgres; take a bundle there first
(`sudo felis db backup`) and restore it onto the host database afterwards if that matters.
Once the move has run for a while, drop the host copy:
`sudo systemctl start postgresql; sudo -u postgres dropdb felis; sudo -u postgres dropuser felis`,
or remove the server package altogether.

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

### Legacy-forwarded backends [VM-VERIFIED]

A 1.8-era backend sits behind ViaVersion, which drops modern forwarding's login plugin
message on the way down to protocol 47, so the proxy has to hand that server the
player's identity BungeeCord-style, in the handshake address. Only the Felis-Legacy
Velocity fork can do that per server. Mark the server's CR and the proxy picks it up at
its next server-list refresh (every 15 s):

```sh
kubectl -n minecraft label minecraftserver <name> felis.lolicon.best/forwarding=legacy
kubectl -n minecraft label minecraftserver <name> felis.lolicon.best/forwarding-   # back to modern
journalctl -u felis-velocity | grep 'legacy forwarding list'
```

The installer's `FELIS_LEGACY_FORWARDING_SERVERS` (default `legacy18`) stays in the list
whatever the labels say. What a label does depends on the proxy the host runs:

| Proxy | A label applies |
|---|---|
| Fork with patch 0004 (`build-velocity.sh` default arm) | from the next connection to that server |
| Fork with 0003 alone (`--deployed`) | at the next `systemctl restart felis-velocity` |
| Stock Velocity | never; the log line is a warning naming the server |

On the test VM (fork with 0004) labelling a server logged `legacy forwarding list is now
[legacy18,resolvecheck]` 12 s later, and removing the label logged the list back to
`[legacy18]`. The fork's own test (`FelisLegacyForwardingTest`) covers the next
connection following the rewritten list.

Legacy forwarding carries no secret. A marked server believes any identity that reaches
its game port, which `felis-allow-game-from-velocity` limits to the proxy and the node
itself; anything else running on the node can reach it too.

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

## 6. Changing the root domain [VM-VERIFIED] [GO-TESTED] [SH-TESTED]

The root domain is written into more places than the installer's config: the panel
certificate (`/etc/felis/panel-tls.crt`), the `felis-config` Secret in both namespaces,
the `felis-api-tls` Secret, the proxy's `felis-link.properties`, the login gate's
`MinecraftServer` env (`FELIS_ROOT_DOMAIN`, `FELIS_PANEL_HOSTNAME`), the Cloudflare tunnel
and DNS. `felis domain set` moves every one of them that lives on the host, in that
order, then restarts what reads them; `felis domain check` reports each surface on its
own line. The installer keeps the installed domain: a rerun with a different
`FELIS_ROOT_DOMAIN` stops and names this command.

```sh
sudo felis domain set new.example.net        # the plan: every surface, what it moves to, what it costs
sudo felis domain set -yes new.example.net   # do it
sudo felis domain check                      # one line per surface; exits 1 while any is behind
```

What it keeps:

- A panel or admin-console hostname set by hand in `[auth]` (anything other than
  `console.<root>` / `op.console.<root>`) stays as it is; change it in
  `/etc/felis/felis.host.toml` yourself if it should move, then run `set` again.
- The other `[auth]` keys (`access_jwt_aud`, `client_ip_header`) and every other line of
  both config files. The edit refuses a file it cannot change line for line (a multi-line
  value, a quoted or dotted key) and names what to fix.
- An operator's own certificate. The installer's self-signed certificate is reissued for
  the new names (same shape, the old pair saved beside it as `*.pre-domain-<time>`); a
  certificate from another issuer that does not cover the new names stops the command
  before anything changes. Replace it with one that does, then run `set` again.

What it costs, which the plan prints before `-yes`:

- **DNS.** `<root>`, `console.<root>`, `op.console.<root>` and `*.<root>` must reach the
  host. The wildcard does not cover `op.console.<root>`, a third-level name: give it its
  own record. `check` resolves each name and warns on the ones that do not resolve yet.
- **Players.** Servers are reached as `<name>.<new root>`; the old addresses stop routing,
  and the proxy restart disconnects everyone online. The first installer re-run after a
  move restarts the proxy once more: the fingerprint it keeps of the proxy's files
  predates the move.
- **Sign-in.** Session cookies belong to the old hostnames, so everyone signs in again.
  Passkeys are bound to the panel hostname: when it changes, the plan counts the passkeys
  that stop working, and their users sign in with an email code and register a new one.
  Without an `[smtp]` relay no code is delivered; an Owner locked out that way recovers
  with `sudo felis breakGlass`.
- **Cloudflare.** The tunnel's ingress and the Access application still carry the old
  names. Re-run the Cloudflare step of `sudo felis setup` after the move; `check` lists
  the tunnel's hostnames against the new ones.
- **A proxy on another host** (a remote `felis-link.properties`) is outside this host's
  reach: `set` prints the three keys to put there.

`set` is safe to repeat: a second run changes only what is still behind, and on an
install that is already on the domain it converges whatever `check` reports. The same
holds after an interruption.

On the reference VM the move from `10.211.55.6.nip.io` to `10-211-55-6.nip.io` took 34
seconds. The certificate served on 30443, `/config.json` on both hostnames, the proxy's
`Felis routing ready: rootDomain=` log line and the login pod's env all carried the new
names afterwards. A second `set -yes` changed and restarted nothing; the installer run
with the old `FELIS_ROOT_DOMAIN` stopped at its first check; a full installer re-run kept
the moved domain and left `check` clean; moving back restored every surface
**[VM-VERIFIED]**.

`check` reads the proxy as behind when `felis-velocity` started before
`felis-link.properties` last changed. Installers before this command rewrote that file
on every run, so a host upgraded from one can show that line once with the file already
on the names; `sudo systemctl restart felis-velocity` clears it. The installer now leaves
the file alone when its content is the same.

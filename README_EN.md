<div align="center">
  <h1 align="center">
    <img src="docs/assets/felis-logo.png" alt="Felis logo" width="270"><br>
    Felis
  </h1>
  <p align="center">
    A Kubernetes-driven Minecraft server hosting platform<br>
    One command to deploy, with automatic lifecycle, backup, and security
    <br><br>
    <a href="README.md">简体中文</a> | <a href="README_EN.md">English</a>
  </p>
</div>

> [!CAUTION]
> **THIS PROJECT IS STILL WIP, YOU SHOULD DO NOT USE THIS PROJECT IN ANY PRODUCTION USAGE. WE ARE NOT RESPOND FOR ANY LEGAL OR HUMANLY PROBLEM.**

<details>
<summary>Table of Contents</summary>

- [Features](#features)
- [Getting Started](#getting-started)
- [Build from Source](#build-from-source)
- [License](#license)
- [Acknowledgements](#acknowledgements)

</details>

## Features

* **On-demand Start and Stop**: A server starts when a player connects to the proxy. The player waits in a queue during start-up and is transferred once the server is ready. Idle servers stop automatically to free memory.

* **Web Dashboard**: Monitor server status, online players, and resource usage from your browser.
  * Console (RCON), whitelist, bans, OPs and LuckPerms permissions
  * File manager: create, delete, rename, chunked upload, download, and unzip while the server is stopped; also used to import worlds
  * Schedules: run commands, restart, stop, start or back up by weekday and time zone, with an in-game warning to players beforehand

* **Backup & Restore**: Enabled by default; the installer renders the archive PVC and its path.
  * Manual backups: archive a server's entire data volume (`/data`, including worlds, configuration, plugins and mods) to the cluster's archive store, with rollback to any backup point
  * Daily restore points: a server played that day gets a restore point once it stops; by default 7 are kept for up to 90 days, rotated separately from manual backups
  * Download and export: download a single backup (with sha256 verification), delete a single backup, or export a whole world
  * Off-site copy (optional): backups are encrypted on the host and synced to S3-compatible storage (AWS S3, Cloudflare R2, Backblaze B2, MinIO and others)
  * Control-plane database: the database holding accounts, server ownership, quotas and the archive index is backed up daily and snapshotted before every upgrade migration; `felis db restore` rolls it back atomically, and the panel's Maintenance & Backups page shows the age of the latest backup (see [troubleshooting §16](docs/troubleshooting.md))

* **Diagnostics**
  * `sudo felis status`: a summary of the node, control plane, game proxy, each server, backups and open alerts
  * `sudo felis doctor`: runs all health checks and lists problems by area with troubleshooting pointers; sends no email
  * `sudo felis support-bundle`: generates a redacted diagnostics archive to attach to support requests (see [troubleshooting §0](docs/troubleshooting.md))
  * Watchdog: runs a check every 2 minutes and emails the platform owners when a problem persists; supports an external heartbeat monitor

* **World Reaper** (optional): Worlds idle for more than 15 days are backed up and then removed to free disk space. Enable it by setting `FELIS_WORLDS_HOST_PATH` at install time (on k3s: `/var/lib/rancher/k3s/storage`); without it, no world is deleted. Expired backups are cleaned up daily regardless of this setting.

* **Multi-core Support**: Compatible with Paper, Fabric, Forge, and NeoForge, accessed through a single Velocity proxy.

* **Modpack Submission**: Players can upload modpacks. After admin approval, each modpack is built automatically and scanned with Trivy; the result is added to the image whitelist and can be selected as a server image.

* **Security**
  * Passkey login: passwordless authentication via fingerprint, face recognition, or hardware security keys
  * Zero-trust access: panel traffic is protected by Cloudflare Access, and the internal API is not exposed to the internet

* **Multi-node Deployment** (experimental, off by default): a single controller node issues all commands, the other nodes run game servers only, and a stopped server can be migrated to another node. Currently available only on the main branch; three-node acceptance testing is not yet complete (see [distributed mode](docs/distributed.md), in Chinese).

## Getting Started

On a prepared Linux host, run:

```bash
curl -fsSL https://raw.githubusercontent.com/FelisMC/Felis/main/deploy/bootstrap.sh | sudo bash
```

The script installs K3s, deploys PostgreSQL and the control plane inside it, and launches a setup wizard. When setup completes, open the configured domain in a browser to reach the control panel.

* **Setup wizard**: The wizard first binds the platform Owner: join the address it shows in Minecraft Java Edition, then enter the 8-character link code that the login server displays (valid for 10 minutes). The step can be skipped and completed later by running `sudo felis setup` again; until an Owner is bound, nobody can sign in to the control panel. The installer launches the wizard automatically only on an interactive terminal; when output is redirected to a log or the install runs under cloud-init, run `sudo felis setup` after it finishes. Setting `FELIS_NO_SETUP=1` makes the installer end at its summary.

* **Supported hosts**: CentOS Stream 9 (aarch64) is verified on physical hardware; Ubuntu 24.04 (x86_64) is tested in CI on every push with a fresh install, a rerun, an upgrade and the install command above (see [operations §1](docs/operations.md#1-supported-hosts)).

* **Preflight checks**: Before modifying the host, the installer checks memory, disk, ports, network range conflicts, existing Kubernetes installations and outbound connectivity. If any check fails, it lists all problems and exits, leaving the host unchanged (see [operations §1](docs/operations.md#1-supported-hosts) for the checks).

* **Upgrading**: Rerun the install command to upgrade felis-api to a newer version; `felis setup` only uses the binary already installed on the host and cannot upgrade it. A rerun keeps the installed root domain, and the release channel must be specified again: hosts that follow the main branch must also set `export FELIS_VERSION_BOOTSTRAP=dev`. A PostgreSQL instance installed on the host by an earlier release is migrated into K3s during the rerun; the original instance on the host is stopped and retained for rollback (see [operations §4](docs/operations.md#4-upgrading-the-pieces-around-felis)).

<details>
<summary>Installation sources and restricted networks</summary>
<br>

A release installation takes the binary, all images and the Velocity plugin from the release assets prebuilt in CI, verifying each against `SHA256SUMS` before import. The host requires no Docker, Gradle or Go, and no access to Docker Hub. If an asset is missing or fails verification, only that image falls back to a local build, and the installer prints a notice (see [troubleshooting §15c](docs/troubleshooting.md)).

The assets can also be copied to the host in advance and installed with `FELIS_ARTIFACT_DIR=<absolute path>`; the Felis binary, images and plugin are then read from that directory. k3s and its images, the JRE, cloudflared, Velocity and the Via plugins are still downloaded from GitHub and PaperMC; hosts with SELinux enabled, such as RHEL, Fedora and openSUSE Leap, additionally install k3s-selinux from rpm.rancher.io; system packages come from the distribution's repositories.

A host with restricted outbound access must therefore allow HTTPS to these addresses or set `https_proxy`. Preflight probes each address before changing the host. Fully offline installation is not yet supported (see [operations §1](docs/operations.md#1-supported-hosts) for the address list).

</details>

## Build from Source

Felis is built with Go and Node.js:

```bash
# Backend (Go 1.26+)
go build -o felis ./cmd/felis

# Frontend (Node.js 22+)
cd panel
npm ci
npm run build

# Docker image
docker build -t felis:custom .
```

## License

This project is licensed under [AGPL-3.0-only](LICENSE).

### License Notes

1. **Derivative works must use AGPL**: Any distribution of this project or of software derived from it must be released under AGPL-3.0 and must include the original copyright notice and license statement.
2. **Network services must also provide source** (AGPL section 13): anyone who offers a modified Felis to others over a network must provide those users with the complete source of the modified version, even without distributing any binary. This is the only substantive difference between AGPL and GPL; as Felis is a hosting platform accessed over a network, this clause applies to virtually every deployment.
3. **Disclaimer**: This project is provided "as is", without warranty of any kind.

## Acknowledgements

* [Kubernetes](https://kubernetes.io/): Container orchestration engine
* [K3s](https://k3s.io/): Lightweight Kubernetes distribution
* [Cloudflare Zero Trust](https://www.cloudflare.com/zero-trust/): Zero trust security infrastructure
* [PostgreSQL](https://www.postgresql.org/): Data persistence
* [React](https://react.dev/): User interface framework
* [Vite](https://vitejs.dev/): Frontend build tool
* [TailwindCSS](https://tailwindcss.com/): CSS framework
* [Bubble Tea](https://github.com/charmbracelet/bubbletea): TUI framework
* [Minecraft](https://www.minecraft.net/): The game this project serves

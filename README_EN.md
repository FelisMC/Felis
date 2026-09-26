# Felis

A Kubernetes-driven Minecraft server hosting platform — one command to deploy, automatic lifecycle, backup, and security.  
一款 Kubernetes 驱动的 Minecraft 服务器托管平台，一行命令部署，自动管理生命周期与安全。

[简体中文](README.md) | [English](README_EN.md)

Table of Contents

- [Features](#features)
- [Getting Started](#getting-started)
- [Build from Source](#build-from-source)
- [License](#license)
- [Acknowledgements](#acknowledgements)

## Features

- **Wake on Join**: Servers start automatically when a player connects, and stop when idle — like hibernate for your server.
- **Web Dashboard**: Monitor server status, online players, and resource usage from your browser, with backup and restore management.
- **Backup & Restore**: One-click snapshots of a server's whole data volume (worlds, config, plugins/mods — the entire /data volume) into the cluster's archive store, with rollback from any backup point — enabled by default (the installer renders the archive PVC and its path).
- **Control-plane database backups**: The database holding accounts, server ownership, quotas and the archive index is backed up daily and snapshotted before every upgrade migrates it; `felis db restore` rolls it back atomically, and the panel's Maintenance & Backups page shows whether the newest backup is fresh (see [troubleshooting §16](docs/troubleshooting.md)).
- **World Reaper** (opt in): Worlds idle for more than 15 days are automatically backed up and removed to free disk space. Enable it by setting `FELIS_WORLDS_HOST_PATH` at install time (on k3s: `/var/lib/rancher/k3s/storage`); without it, no world is ever deleted.
- **Multi-core Support**: Compatible with Paper, Fabric, Forge, and NeoForge, federated behind a Velocity proxy.
- **Modpack Submission**: Players submit custom modpacks; admin approval triggers an automatic build, and the result is whitelisted as a server image you can select to deploy.
- **Passkey Login**: Passwordless authentication via fingerprint, face recognition, or hardware security keys.
- **Zero Trust Security**: Panel traffic protected by Cloudflare Access; the internal API is never exposed to the internet.

## Getting Started

On a prepared Linux host, run (the verified distributions and architectures are listed in
[operations §1](docs/operations.md#1-supported-hosts): CentOS Stream 9 on aarch64 is verified
on a real host, and Ubuntu 24.04 on x86_64 gets a fresh install, rerun and upgrade in CI on
every push):

```bash
curl -fsSL https://raw.githubusercontent.com/FelisMC/Felis/main/deploy/bootstrap.sh | sudo bash
```

The script installs K3s, deploys PostgreSQL and the control plane inside it, and launches a setup wizard. Once done, open your browser at the configured domain. A PostgreSQL an earlier release installed on the host is moved into K3s on the next rerun, and the host copy is stopped and kept for a rollback (see [Operations §4](docs/operations.md#4-upgrading-the-pieces-around-felis)).

Before it changes anything, the script checks RAM, disk, ports, network-range clashes, any Kubernetes already there and outbound access, lists every problem at once and stops with the host untouched (the checks are in [operations §1](docs/operations.md#1-supported-hosts)).

> **This repository is currently private**, so the command above returns 404. Use the
> credentialed form instead; the installer itself needs the same token to resolve and
> download the release, so pass it through with `sudo -E`:
>
> ```bash
> export FELIS_GITHUB_TOKEN=<a token with read access to this repository>
> printf 'header = "Authorization: Bearer %s"\n' "$FELIS_GITHUB_TOKEN" \
>   | curl -fsSL --config - -H "Accept: application/vnd.github.raw" \
>       https://api.github.com/repos/FelisMC/Felis/contents/deploy/bootstrap.sh \
>   | sudo -E bash
> ```
>
> The token reaches `curl --config -` over stdin instead of the command line: argv is
> readable by any local user via `/proc`, and that is exactly why the installer's
> internal `github_api` uses the same form.

Rerunning this command is also how you upgrade felis-api to a newer version (`felis setup`
cannot — it uses the binary already installed on the host). The rerun keeps the installed
root domain but **not** the channel: if this host follows main, also
`export FELIS_VERSION_BOOTSTRAP=dev`.

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

The source code is released under [AGPL-3.0-only](LICENSE).

### License Notes

1. **Derivative works are AGPL too**: Any distribution of this project or of software derived from it must be released under AGPL-3.0 and must include the original copyright notice and license statement.
2. **Running it as a network service also triggers the source obligation** (AGPL section 13): if you host a modified Felis for other people to use, you must offer those users the complete source of your modified version — even if you never distribute a binary. This is the one substantive difference between AGPL and GPL, and since Felis is a hosting platform reached over a network, it will essentially always apply.
3. **Disclaimer**: This project is provided "as is", without warranty of any kind.

## Acknowledgements

- [Kubernetes](https://kubernetes.io/): Container orchestration engine
- [K3s](https://k3s.io/): Lightweight Kubernetes distribution
- [Cloudflare Zero Trust](https://www.cloudflare.com/zero-trust/): Zero trust security infrastructure
- [PostgreSQL](https://www.postgresql.org/): Data persistence
- [React](https://react.dev/): User interface framework
- [Vite](https://vitejs.dev/): Frontend build tool
- [TailwindCSS](https://tailwindcss.com/): CSS framework
- [Bubble Tea](https://github.com/charmbracelet/bubbletea): TUI framework
- [Minecraft](https://www.minecraft.net/): What makes this all worthwhile

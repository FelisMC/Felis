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
- **Backup & Restore**: One-click world snapshots into the cluster's archive store, with rollback from any backup point — enabled by default (the installer renders the archive PVC and its path).
- **World Reaper** (opt in): Worlds idle for more than 15 days are automatically backed up and removed to free disk space. Enable it by setting `FELIS_WORLDS_HOST_PATH` at install time (on k3s: `/var/lib/rancher/k3s/storage`); without it, no world is ever deleted.
- **Multi-core Support**: Compatible with Paper, Fabric, Forge, and NeoForge, federated behind a Velocity proxy.
- **Modpack Submission**: Players submit custom modpacks; admin approval triggers automatic build and deployment.
- **Passkey Login**: Passwordless authentication via fingerprint, face recognition, or hardware security keys.
- **Zero Trust Security**: Panel traffic protected by Cloudflare Access; the internal API is never exposed to the internet.

## Getting Started

On a prepared Linux host, run:

```bash
curl -fsSL https://raw.githubusercontent.com/MliroLirrorsIngenuity/Felis/main/deploy/bootstrap.sh | sudo bash
```

The script installs K3s, deploys the control plane, and launches a setup wizard. Once done, open your browser at the configured domain.

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

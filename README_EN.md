# Felis

A Kubernetes-driven Minecraft server hosting platform — one command to deploy, automatic lifecycle, backup, and security.  
一个 Kubernetes 驱动的 Minecraft 服务器托管平台，一行命令部署，自动管理服务器启动、备份与安全。

[简体中文](README.md) | [English](README_EN.md)

## Table of Contents

- [Features](#features)
- [Getting Started](#getting-started)
- [Build from Source](#build-from-source)
- [License](#license)
- [Acknowledgements](#acknowledgements)

## Features

- **Wake on Join**: Servers start automatically when a player connects, and stop when idle — like hibernate for your server.
- **Web Dashboard**: Monitor server status, manage backups, approve modpack submissions, and manage player accounts from your browser.
- **Auto Backup & Restore**: Scheduled world backups with one-click restore from any backup point.
- **World Reaper**: Worlds idle for more than 15 days are automatically backed up and removed to free disk space.
- **Multi-core Support**: Compatible with Paper, Fabric, Forge, and NeoForge, federated behind a Velocity proxy.
- **Modpack Submission**: Players submit custom modpacks; admin approval triggers automatic build and deployment.
- **Passkey Login**: Passwordless authentication via fingerprint, face recognition, or hardware security keys.
- **Zero Trust Security**: Cloudflare Access JWT + Tunnel secures the panel; the internal API is never exposed to the internet.

## Getting Started

### Requirements

- Linux host (amd64, Kernel ≥ 5.4)
- 4 GB RAM minimum, 8 GB recommended
- 20 GB disk minimum
- root or sudo privileges

### One-command Install

```bash
curl -fsSL https://raw.githubusercontent.com/MliroLirrorsIngenuity/Felis/main/deploy/bootstrap.sh | sudo bash
```

The script automatically installs K3s, deploys the Felis control plane, and launches a TUI setup wizard to guide you through domain configuration, admin account setup, and more.

### Creating a Server

1. Open your browser and navigate to the configured domain
2. Go to the Servers page and click "Create Server"
3. Choose the core type and version, set memory limits and subdomain
4. Click create — the system auto-builds the image and launches the server

### Player Onboarding

Players add the Velocity proxy address to their Minecraft client to join the lobby. Use the `/link` command to get a binding code, then complete account linking in the dashboard.

## Build from Source

Felis is built with Go and Node.js:

```bash
# Backend (Go 1.26+)
go build -o felis ./cmd/felis

# Frontend panel (Node.js 22+)
cd panel
npm ci
npm run build

# Docker image
docker build -t felis:custom .
```

See [CONTRIBUTING.md](./CONTRIBUTING.md) for more details.

## License

The source code is released under the MIT License.

### License Notes

1. **Attribution**: Any distribution of this project or derivative works must include the original copyright notice and license statement.
2. **Disclaimer**: This project is provided "as is", without warranty of any kind.

## Acknowledgements

- [Kubernetes](https://kubernetes.io/) / [K3s](https://k3s.io/) — the underlying orchestration engine
- [Cloudflare Zero Trust](https://www.cloudflare.com/zero-trust/) — zero trust security infrastructure
- [PostgreSQL](https://www.postgresql.org/) — data persistence
- [React](https://react.dev/) + [Vite](https://vitejs.dev/) + [TailwindCSS](https://tailwindcss.com/) — frontend stack
- [Bubble Tea](https://github.com/charmbracelet/bubbletea) — TUI framework
- [Minecraft](https://www.minecraft.net/) — what makes this all worthwhile

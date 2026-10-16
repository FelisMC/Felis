<div align="center">
  <h1 align="center">
    <img src="assets/felis-logo.png" alt="Felis logo" width="270"><br>
    Felis
  </h1>
  <p align="center">
    A Kubernetes-based Minecraft server hosting platform<br>
    One command to deploy; server start and stop, backups and player access are managed automatically
    <br><br>
    <a href="README.md">简体中文</a> | English
    <br>
    <a href="https://felismc.com/">Website</a> | <a href="https://docs.felismc.com/en/">Documentation</a>
  </p>
</div>

> [!CAUTION]
> **当前仅提供开发快照，已撤下公开 Release。仅供开发者在隔离的测试环境中验证，不建议普通用户部署。**
> **此项目仍处于早期开发阶段，您不该在任何生产环境使用该项目。若产生任何问题，贵用户的使用行为与 FelisMC 团队无任何民事刑事法律关系。**<br>
> **THIS PROJECT IS STILL WIP, YOU SHOULD DO NOT USE THIS PROJECT IN ANY PRODUCTION USAGE. WE ARE NOT RESPOND FOR ANY LEGAL OR HUMANLY PROBLEM.**

Felis is a self-hosted Minecraft server hosting platform. A single install command deploys k3s, PostgreSQL, the control plane and the Velocity game proxy on a Linux host. From then on, creating servers, starting and stopping them, backups and player access are handled in the web panel, and routine host operations can be run from the panel too.

<details>
<summary>Contents</summary>

- [Features](#features)
- [How it works](#how-it-works)
- [Getting started](#getting-started)
- [Documentation](#documentation)
- [Building from source](#building-from-source)
- [License](#license)
- [Acknowledgements](#acknowledgements)

</details>

## Features

- **One-command deployment**: the installer checks memory, disk, ports and network before it changes the host, then installs k3s, the database, the control plane and the game proxy. A setup wizard guides you through the panel's access method and storage and creates the first Owner.
- **On-demand start and stop**: a player's connection wakes the target server; the player waits in the lobby and is moved in once the server is ready. Servers with nobody online go to sleep after a set time and free their memory.
- **Web panel**: manage the console (RCON), whitelist, bans, OP and LuckPerms permissions in the browser. The file manager supports chunked uploads, downloads and world imports; scheduled tasks run commands, restarts, starts, stops or backups and warn players in game beforehand.
- **Player access**: every player joins through one Velocity game proxy, with Mojang and third-party Yggdrasil authentication sources. The login space handles authentication and account linking, the lobby offers a server menu, and the player entry policy is set in the panel.
- **Backup and restore**: manual backups, live backups and daily restore points are on by default, with backup downloads and world exports. The control-plane database is backed up daily; backups can be encrypted and synced to S3-compatible storage and used to rebuild the whole host on a new machine.
- **Images and modpacks**: servers can use only images on the whitelist that Admins maintain. Users can submit modpacks; once an Admin approves one, it is built automatically, scanned by Trivy and added to the whitelist.
- **Operations and alerts**: **Maintenance & Backups** in the panel gathers host status, open alerts, health checks, support bundles and updates. Host operations such as the access method, root domain, credential rotation and the off-site copy run from the panel and are recorded in the audit log. A watchdog checks the host regularly and mails the Owners when something is wrong, with an external heartbeat and Prometheus metrics.
- **Security**: passkey and email code sign-in; sensitive operations require identity verification and are recorded in the audit log. Game servers are network-isolated from each other, and the internal API is not exposed to the internet. The panel can be reached through your own reverse proxy or through Cloudflare Tunnel and Access.
- **Multi-node (experimental)**: a controller node directs several worker nodes, with stopped-server migration, failover and controller high availability.

## How it works

- **Servers**: each Minecraft server runs in k3s as a Kubernetes resource, with its world on its own data volume. Admins create servers; users link their game identity, then claim and manage their own servers.
- **Players**: players connect to the game proxy, authenticate in the login space and then enter the target server. When the target is asleep, Felis wakes it and the player waits in the lobby.
- **Data**: accounts, server ownership, quotas and the backup index are stored in PostgreSQL running in k3s. World backups are kept in the cluster's archive store and can be synced to an off-site bucket.
- **Host**: the `felis` command and a few systemd services take care of database backups, the watchdog, off-site sync and host operations. Host operations started in the panel are run by a root service on the host with the matching command.

## Getting started

**Requirements**

- a 64-bit Linux host (`x86_64` or `aarch64`) with systemd and root access;
- CentOS Stream 9, RHEL 9, Rocky Linux 9, AlmaLinux 9, Fedora, Ubuntu, Debian 12, openSUSE or Arch Linux; the reference platforms are CentOS Stream 9 and Ubuntu 24.04 LTS;
- at least 2 vCPU, 4 GB of RAM (plus 2 GB of swap) and 40 GB of disk, enough for about 20 concurrent players.

See [Requirements](https://docs.felismc.com/en/guide/requirements) for the full list.

**Install**

Only the development channel is available (it follows the `main` branch and builds from source). On an isolated test host, run:

```bash
curl -fsSL https://raw.githubusercontent.com/FelisMC/Felis/main/deploy/bootstrap.sh | sudo FELIS_VERSION_BOOTSTRAP=dev bash
```

When the installer asks what to install, choose `[1] Felis`. After the installation, the setup wizard guides you through the panel's access method and storage and prints a one-time web setup link for the first Owner. Open it in a browser, register an email address and create a passkey to enter the panel. Installer variables, offline installation and post-install checks are covered in [Installation](https://docs.felismc.com/en/guide/deployment).

## Documentation

The full documentation is at [docs.felismc.com](https://docs.felismc.com/en/), in English and Simplified Chinese.

- **Getting started**: [Requirements](https://docs.felismc.com/en/guide/requirements) · [Installation](https://docs.felismc.com/en/guide/deployment)
- **User guide**: [Managing servers](https://docs.felismc.com/en/guide/servers) · [Player access](https://docs.felismc.com/en/guide/players) · [Platform administration](https://docs.felismc.com/en/guide/administration)
- **Operations**: [Overview](https://docs.felismc.com/en/operations/) · [Backup and restore](https://docs.felismc.com/en/operations/backup) · [Upgrading](https://docs.felismc.com/en/operations/upgrade) · [Monitoring and alerts](https://docs.felismc.com/en/operations/monitoring) · [Troubleshooting](https://docs.felismc.com/en/operations/troubleshooting) · [Multi-node and high availability](https://docs.felismc.com/en/guide/distributed)
- **Reference**: [Command line](https://docs.felismc.com/en/reference/cli) · [Configuration](https://docs.felismc.com/en/reference/configuration) · [API](https://docs.felismc.com/en/reference/api) · [Architecture](https://docs.felismc.com/en/reference/architecture) · [Known limitations](https://docs.felismc.com/en/reference/limitations)
- **Development**: [Building from source](https://docs.felismc.com/en/guide/building) · [Contributing](https://docs.felismc.com/en/reference/contributing)

## Building from source

The backend is written in Go; the panel uses React and Vite:

```bash
# Backend
go build -o felis ./cmd/felis

# Panel
cd panel
npm ci
npm run build
```

Toolchain versions, building the images and plugins, and running the tests are covered in [Building from source](https://docs.felismc.com/en/guide/building).

## License

Felis is licensed under [AGPL-3.0-only](LICENSE).

1. **Derivative works must use the AGPL**: when you distribute copies of Felis or software derived from it, you must release them under AGPL-3.0 and keep the original copyright and license notices.
2. **Network services must provide the source as well** (AGPL section 13): when you offer a modified Felis to others over a network, you must give those users the complete modified source, even if you distribute no binaries. Felis is a hosting platform accessed over the network, so this clause applies to nearly every deployment.
3. **Disclaimer**: Felis is provided "as is", and the authors accept no legal liability arising from its use.

See [License](https://docs.felismc.com/en/reference/license).

## Acknowledgements

- [Kubernetes](https://kubernetes.io/): container orchestration
- [k3s](https://k3s.io/): lightweight Kubernetes distribution
- [Velocity](https://papermc.io/software/velocity): Minecraft proxy server
- [PostgreSQL](https://www.postgresql.org/): data persistence
- [Cloudflare Zero Trust](https://www.cloudflare.com/zero-trust/): zero-trust access
- [React](https://react.dev/), [Vite](https://vitejs.dev/) and [Tailwind CSS](https://tailwindcss.com/): the panel front end
- [Bubble Tea](https://github.com/charmbracelet/bubbletea): terminal user interface framework
- [Minecraft](https://www.minecraft.net/): the game Felis serves

# Felis Sequence Diagrams

This file carries the spec section 28 sequence-diagram deliverables that are not
covered by the OpenAPI artifact.

## Section 28 #9: Ping To Join To Wake To Ready To Teleport

```mermaid
sequenceDiagram
    autonumber
    actor Player
    participant Velocity as Velocity proxy
    participant Registry as Velocity server registry
    participant API as felis-api internal face
    participant Cluster as MinecraftServer CRD/status
    participant Operator as felis operator
    participant Backend as Minecraft backend

    Player->>Velocity: server-list ping for subdomain.root-domain
    Velocity->>Registry: read cached lifecycle view
    Registry-->>Velocity: phase-aware MOTD
    Velocity-->>Player: ping response (read-only, no wake)

    Player->>Velocity: join subdomain.root-domain
    Velocity->>Registry: resolve host to server
    Registry-->>Velocity: ServerView(name, ready=false)

    alt backend already ready and registered
        Velocity-->>Player: initial server = backend
        Player->>Backend: connect
    else backend not ready and lobby configured
        Velocity-->>Player: initial server = lobby
        Velocity->>API: POST /api/v1/internal/servers/{name}/wake {mc_uuid}
        API->>Cluster: GetServer(name)
        API->>API: authorize autostartPolicy, cooldown, running cap
        API->>Cluster: SetDesiredState(name, Running)
        API-->>Velocity: 202 phase/ready
        Velocity->>Velocity: enqueue waiter
        Operator->>Cluster: reconcile DesiredState=Running
        Operator->>Backend: start pod/service
        Backend-->>Operator: RCON-ready / lifecycle ready
        Operator-->>Cluster: status.ready=true
        loop every waiting tick
            Velocity->>API: GET /api/v1/internal/servers/{name}/status
            API->>Cluster: GetServer(name)
            API-->>Velocity: ready flag
        end
        Velocity->>Registry: lookup registered backend
        Velocity-->>Player: "ready - moving you in"
        Velocity->>Player: Connect request to backend
        Player->>Backend: connect
        Velocity->>API: POST /api/v1/internal/servers/{name}/join-event {mc_uuid}
        API->>API: RecordJoin; refresh activity and allowlist UUID
        API-->>Velocity: 204
    else backend not ready and no lobby configured
        Velocity-->>Player: disconnect with reconnect-later message
        Velocity->>API: POST /api/v1/internal/servers/{name}/wake {mc_uuid}
        API->>Cluster: SetDesiredState(name, Running) if authorized
        API-->>Velocity: 202 or branchable error
    end
```

## Section 28 #11: Claim Transaction

```mermaid
sequenceDiagram
    autonumber
    actor Player
    participant Panel as Web panel
    participant API as felis-api external face
    participant Repo as Repo / Postgres
    participant Audit as Audit log

    Player->>Panel: click Claim on ownerless server
    Panel->>API: POST /api/v1/servers/{name}/claim
    API->>API: validate server name and principal
    API->>Repo: IsLinked(user_id)
    alt user has no verified account link
        Repo-->>API: false
        API-->>Panel: 412 not_linked
    else linked
        Repo-->>API: true
        API->>Repo: QuotaAvailable(user_id)
        alt quota exhausted
            Repo-->>API: false
            API-->>Panel: 403 quota_exceeded
        else quota available
            Repo-->>API: true
            API->>Repo: ClaimServer(name, user_id)
            Note over Repo: SELECT EXISTS(server); then atomic UPDATE servers SET owner_id=$2, claimed_at=now() WHERE name=$1 AND owner_id IS NULL AND deleted_at IS NULL
            alt server missing
                Repo-->>API: ErrNotFound
                API-->>Panel: 404 not_found
            else zero rows affected
                Repo-->>API: claimed=false
                API-->>Panel: 409 already_claimed
            else one row affected
                Repo-->>API: claimed=true
                API->>Audit: external claim audit
                API-->>Panel: 200 {"claimed":true}
            end
        end
    end
```

## Section 28 #12: Account Binding /link Flow

```mermaid
sequenceDiagram
    autonumber
    actor Player
    participant Game as Minecraft server or Velocity
    participant LinkClient as Felis LinkClient
    participant APIInternal as felis-api internal face
    participant Repo as Repo / Postgres
    participant Panel as Web panel
    participant APIExternal as felis-api external face

    Player->>Game: /link
    Game->>Game: read verified online-mode UUID
    Game->>LinkClient: requestCode(mc_uuid)
    LinkClient->>APIInternal: POST /api/v1/internal/account/link/code {mc_uuid}
    APIInternal->>APIInternal: validate UUID; default auth_source=mojang if absent; generate 8-symbol code
    APIInternal->>Repo: CreateLinkCode(code, mc_uuid, auth_source, expires_at)
    Repo-->>APIInternal: inserted account_link_codes row
    APIInternal-->>LinkClient: 201 {code, expires_at}
    LinkClient-->>Game: LinkCode
    Game-->>Player: show one-time code in chat

    Player->>Panel: open Account link flow
    Panel->>APIExternal: POST /api/v1/account/link/start
    APIExternal->>Repo: IsLinked(user_id)
    Repo-->>APIExternal: linked status
    APIExternal-->>Panel: status and "run /link" instructions

    Player->>Panel: submit code
    Panel->>APIExternal: POST /api/v1/account/link/verify {code}
    APIExternal->>APIExternal: trim and uppercase code
    APIExternal->>Repo: VerifyLinkCode(user_id, code, now)
    Repo->>Repo: SELECT non-expired code
    alt missing or expired code
        Repo-->>APIExternal: ErrLinkCodeInvalid
        APIExternal-->>Panel: 400 invalid_code
    else UUID linked to another user
        Repo-->>APIExternal: ErrConflict
        APIExternal-->>Panel: 409 already_linked
    else valid code
        Repo->>Repo: INSERT account_links(user_id, mc_uuid, auth_source) ON CONFLICT (user_id, mc_uuid) DO UPDATE auth_source
        Repo->>Repo: DELETE account_link_codes WHERE code=$1
        Repo-->>APIExternal: mc_uuid, auth_source
        APIExternal->>Repo: Audit account.link
        APIExternal-->>Panel: 200 {linked:true, mc_uuid, auth_source}
    end
```

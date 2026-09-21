# telnet-central

Central control API for your fleet of `telnetAgent` services.

```
client --REST--> telnet-central --HTTP--> telnetAgent (every host in the group(s))
                      |
                  PostgreSQL (hosts, groups, membership)
```

## Run

```bash
cp .env.example .env        # set DB_HOST, DB_USER, DB_PASSWORD, DB_NAME ...
go mod tidy                 # first time only: fetches pgx and godotenv
go run .
```

The database must already exist (`CREATE DATABASE telnet_central;`). Tables are
created automatically on startup from `schema.sql`.

Build for Linux from any OS: `GOOS=linux GOARCH=amd64 go build -o telnet-central .`

## Run with Docker

```bash
cp .env.example .env        # set DB_USER, DB_PASSWORD, DB_NAME (DB_PASSWORD is required)
go mod tidy                 # once, so go.sum exists for the image build
docker compose up -d --build
docker compose logs -f api
```

This starts PostgreSQL (data in the `pgdata` volume) and the API on `http://localhost:8080`
(change with `API_PORT`). Compose points the API at the `db` container by itself, so `DB_HOST`
in `.env` is ignored under Docker and can stay `localhost` for `go run .`. Credentials in `.env`
are used both to create the database and to connect to it.

Useful commands:

```bash
docker compose ps                    # status and health
docker compose down                  # stop, keep data
docker compose down -v               # stop and DELETE the database volume
docker compose up -d --build api     # rebuild after code changes
```

The agents are reached over your network from inside the container, so each agent IP must be
routable from the Docker host. To test against an agent running on the Docker host itself,
register the host's LAN IP, not `127.0.0.1` (inside the container that is the container).
To use an external PostgreSQL instead, remove the `db` service and `depends_on`, and set
`DB_HOST` for the `api` service.

## Configuration (.env)

| Variable          | Default          | Meaning                                              |
|-------------------|------------------|------------------------------------------------------|
| `LISTEN_ADDR`     | `:8080`          | Address the central API listens on                   |
| `DB_HOST`         | `localhost`      | PostgreSQL host                                      |
| `DB_PORT`         | `5432`           | PostgreSQL port                                      |
| `DB_USER`         | `postgres`       | PostgreSQL user                                      |
| `DB_PASSWORD`     |                  | PostgreSQL password                                  |
| `DB_NAME`         | `telnet_central` | Database name                                        |
| `DB_SSLMODE`      | `disable`        | `disable`, `require`, `verify-full`, ...             |
| `AGENT_PORT`      | `29900`          | Port telnetAgent listens on                          |
| `AGENT_TIMEOUT`   | `15s`            | Per-agent HTTP timeout                               |
| `MAX_CONCURRENCY` | `50`             | Max agent calls in flight at once                    |
| `API_KEY`         | (empty)          | If set, requests need `X-API-Key` or `Bearer` header |

Real environment variables win over `.env`, so you can also configure it from systemd.

## Endpoints

### Hosts

| Method | Path                                | Body                                             | Notes |
|--------|-------------------------------------|--------------------------------------------------|-------|
| POST   | `/hosts`                            | `{"ip"\|"ips":[...], "groups":[...], "description":""}` | Add one or many IPs, optionally into groups. Idempotent. Groups must exist. |
| GET    | `/hosts`                            |                                                  | List all. `?group=web` filters by group. |
| GET    | `/hosts/{ip}`                       |                                                  | One host with its groups. |
| PATCH  | `/hosts/{ip}`                       | `{"description":"..."}`                          | |
| DELETE | `/hosts/{ip}`                       |                                                  | Also removes it from **every** group. |
| POST   | `/hosts/{ip}/groups`                | `{"groups":["web","prod"]}`                      | One IP into many groups. |
| DELETE | `/hosts/{ip}/groups/{group}`        |                                                  | Remove from one group. |

### Groups

| Method | Path                                | Body                                             | Notes |
|--------|-------------------------------------|--------------------------------------------------|-------|
| POST   | `/groups`                           | `{"name":"web","description":"","hosts":[...]}`  | `hosts` optional, must already exist. 409 if name taken. |
| GET    | `/groups`                           |                                                  | All groups with their hosts. |
| GET    | `/groups/{group}`                   |                                                  | |
| PATCH  | `/groups/{group}`                   | `{"name":"new","description":"..."}`             | Both optional. Rename keeps members. |
| DELETE | `/groups/{group}`                   |                                                  | Deletes the group only, hosts stay. |
| POST   | `/groups/{group}/hosts`             | `{"hosts":["10.0.0.1","10.0.0.2"]}`              | Many IPs into one group. Hosts must exist. |
| DELETE | `/groups/{group}/hosts/{ip}`        |                                                  | Remove one IP from the group. |

### Telnet check

`POST /telnet/{groups}` where `{groups}` is one group or a comma-separated list
(`/telnet/web` or `/telnet/web,db`). Agents that are in several of the groups
are only called once.

```json
{"dest": "10.20.30.40", "ports": [22, 443]}
```

Every agent runs `dest:port` for every port. The response is always `200` if the
request was valid, with one result per agent per port:

```json
{
  "groups": ["web"],
  "dest": "10.20.30.40",
  "ports": [22, 443],
  "agents": 2,
  "summary": {"total": 4, "reachable": 3, "unreachable": 1},
  "results": [
    {"agent": "10.0.0.1", "dest": "10.20.30.40", "port": 22,  "reachable": true,  "result": "Connected", "src": "10.0.0.1", "duration_ms": 8},
    {"agent": "10.0.0.1", "dest": "10.20.30.40", "port": 443, "reachable": false, "result": "Failed: dial tcp 10.20.30.40:443: connect: connection refused", "src": "10.0.0.1", "duration_ms": 3},
    {"agent": "10.0.0.2", "dest": "10.20.30.40", "port": 22,  "reachable": false, "agent_error": "agent unreachable: ...", "duration_ms": 15001}
  ]
}
```

- `reachable` is true only when the agent answered `Connected`.
- `result` is the agent's own answer (`Failed: ...` / `The connection timed out.`).
- `agent_error` means the *agent itself* could not be reached or misbehaved, which is
  different from the target port being closed.

Limits: 50 ports and 2000 checks (agents x ports) per request.

## Examples

```bash
API=http://localhost:8080

# groups
curl -X POST $API/groups -d '{"name":"web","description":"web tier"}'
curl -X POST $API/groups -d '{"name":"db"}'

# add many IPs straight into groups
curl -X POST $API/hosts -d '{"ips":["10.0.0.1","10.0.0.2","10.0.0.3"],"groups":["web"]}'
curl -X POST $API/hosts -d '{"ip":"10.0.1.1","groups":["db","web"]}'

# add an existing IP to more groups / many IPs to one group
curl -X POST $API/hosts/10.0.0.1/groups -d '{"groups":["db"]}'
curl -X POST $API/groups/db/hosts       -d '{"hosts":["10.0.0.2","10.0.0.3"]}'

# telnet check from every agent in web AND db
curl -X POST $API/telnet/web,db -d '{"dest":"10.20.30.40","ports":[22,443]}'

# delete an IP (leaves every group automatically)
curl -X DELETE $API/hosts/10.0.0.1

# with an API_KEY configured
curl -H "X-API-Key: $KEY" $API/groups
```

## Notes

- **The agent has no authentication.** Anyone who can reach port 29900 on a host can make
  it open TCP connections. Restrict that port (firewall / security group) to the machine
  running telnet-central. Set `API_KEY` here too, since this API can trigger the same thing.
- Hosts are identified by IP and groups by name, so paths look like `/hosts/10.0.0.1` and
  `/groups/web`. IPv6 works too. Group names: letters, digits, `.`, `_`, `-` (max 63).
- To change a host's IP, delete it and add the new one.

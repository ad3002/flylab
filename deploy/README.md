# Production deployment (flylab.aglabx.com)

FlyLab runs as a native binary under a dedicated unprivileged system user, behind nginx with a
Let's Encrypt certificate. No Docker is involved in production.

## Layout on the host

| Path | Purpose |
| --- | --- |
| `/mnt/beta/flylab/` | home of system user `flylab` (shell bash, no password; `su - flylab` from root) |
| `/mnt/beta/flylab/app/` | git checkout of this repository (`main`) |
| `/mnt/beta/flylab/app/bin/{flysim,flylab}` | release binaries built by `make setup` as user `flylab` |
| `/mnt/beta/flylab/app/data/` | FlyWire csv + parquet, `cache/flywire_630_csr.bin` built by `scripts/setup_data.sh` |
| `/mnt/beta/flylab/db/flylab.db` | SQLite store (WAL) |
| `/mnt/beta/flylab/artifacts/` | per-job artifacts and export zips |
| `/mnt/beta/flylab/flylab.env` | environment file (mode 640), see `.env.example` for the variable list |
| `/mnt/beta/flylab/.cargo`, `/mnt/beta/flylab/go` | user-local Rust toolchain (rustup) and Go toolchain cache |
| `/etc/systemd/system/flylab.service` | copy of `deploy/flylab.service` |
| `/etc/nginx/sites-available/flylab.aglabx.com` | copy of `deploy/nginx-flylab.aglabx.com.conf`, then extended by certbot |
| `/usr/local/sbin/flylab-update` | root-owned copy of `deploy/update.sh` |

The service listens on `127.0.0.1:8107`; nginx terminates TLS and proxies to it. The
natural-language planner runs the Claude Code CLI (`/usr/bin/claude -p`, about 3-5 s per parse)
as user `flylab`, using that user's Claude login in `/mnt/beta/flylab/.claude`. If the binary is
missing, `/plans/parse` falls back to a keyword parser and every response carries `llm_error`
(visible in the UI); any other Claude failure is a `502 LLM_ERROR`. Tunables: `CLAUDE_BIN`,
`CLAUDE_MODEL`, `CLAUDE_TIMEOUT_SECONDS`, `LLM_MAX_CONCURRENCY`, `PARSE_RATE_LIMIT_PER_HOUR`,
`REGISTRATION_OPEN` (see `.env.example`). Abuse limits (per-IP/per-username login throttling,
per-IP registration, per-IP and global parse budgets, password-hash concurrency) are listed in
`README.md` and `docs/v2_contract.md` section 10; the client address is nginx's `X-Real-IP`.
Open registration hands every visitor a parse quota on the operator's Claude login, so once the
accounts you need exist, consider `REGISTRATION_OPEN=false`. Accounts can be created on the host with
`su - flylab -c 'cd app && DB_PATH=/mnt/beta/flylab/db/flylab.db bin/flylab user create --username U --password P'`.

## Updating

```bash
sudo flylab-update
```

It pulls `main` fast-forward, rebuilds both binaries as user `flylab`, restarts the service and
fails loudly if `/health` does not report `status: ok` within 30 s.

## First-time setup (summary)

1. `useradd --system --create-home --home-dir /mnt/beta/flylab --shell /bin/bash --user-group flylab`
2. As `flylab`: install rustup (`--profile minimal`), `git clone` the repo into `app/`,
   copy the two FlyWire data files into `app/data/`, run `make setup`, `./scripts/setup_data.sh`,
   `make test`, `./scripts/smoke.sh`.
3. Write `/mnt/beta/flylab/flylab.env` with absolute paths (see table above), `chmod 640`.
4. As root: install `deploy/flylab.service`, `systemctl enable --now flylab`; install the nginx
   vhost, `nginx -t`, reload; `certbot --nginx -d flylab.aglabx.com --redirect`; copy
   `deploy/update.sh` to `/usr/local/sbin/flylab-update`.

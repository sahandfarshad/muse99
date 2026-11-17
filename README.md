# Muse99 — a production agent like Muse

Give it a task and it works till the end. Give it a server only if you want it to — then it can work there, for free, on your own machine.

**Repo:** https://github.com/sahfar363/muse99

Muse99 is not the Muse app itself — that has hosted Gmail/Calendar, wallet, proactivity and Meta's runtime. Muse99 is the full, reproducible production core: same persona, memory, goals, autonomous task loop, sub-agents, tools, web dashboard, SSH menu, remote servers, scheduler, and packaging. Point it at Spark and it is Muse mode. Point it at any other OpenAI-compatible API and it keeps the same behaviour with a different brain.

## Quick install — Ubuntu server, one line, no compile

```bash
curl -fsSL https://raw.githubusercontent.com/sahfar363/muse99/main/install.sh | bash
```

That downloads the pre-built binary for your server (amd64 or arm64) from the latest GitHub release and puts it in `~/.local/bin/muse99`. No Go, no clone, no build on the server — GitHub Actions already built it when the release was tagged. Then:

```bash
export PATH="$HOME/.local/bin:$PATH"
export MODEL_API_KEY="your-key"
export MODEL_API_BASE_URL="https://api.meta.ai/v1"
export MODEL_ID="muse-spark-1.3"   # or muse-spark-1.4 when it ships
muse99 --allow-shell
```

Specific version, or a different install folder:

```bash
curl -fsSL https://raw.githubusercontent.com/sahfar363/muse99/main/install.sh | bash -s -- --version v0.1.0
VERSION=v0.1.0 INSTALL_DIR=$HOME/bin bash install.sh
```

If no release exists yet, the installer stops with a clear error and tells you to create one (`git tag v0.1.0 && git push origin v0.1.0`) — it never silently falls back to compiling.

## SSH menu — web address on top

Just run `muse99` over SSH:

```
═══════════════════════════════════════════════════════════
  ✦ Muse99  —  Web UI: http://192.168.1.10:8080
  Status: web UI is stopped  ·  choose 1 to start it
  Provider: spark  ·  Model: muse-spark-1.3  ·  Muse Spark — Muse mode
  User: Takahashi  ·  Workdir: /home/ubuntu
═══════════════════════════════════════════════════════════

  1  Start web UI
  2  Terminal chat
  3  Stop web UI
  4  Restart web UI
  5  Choose web port
  6  Set domain
  7  Set up SSL / Let's Encrypt
  8  Update Muse99
  9  Providers / API keys
  10 Show memory
  11 Show web address / status
  12 Quit
```

Everything the server needs is in the menu: start / stop / restart the web UI, change its port, set a domain, set up SSL with Let's Encrypt (it checks for certbot and for existing certificates first), update Muse99 itself to the latest release, and switch API providers. The web link on top shows `https://your-domain` once a domain and SSL are set, otherwise `http://<server-ip>:<port>`.

## Web UI — clean dashboard

```bash
muse99 --web --port 8080 --allow-shell
# open http://your-server-ip:8080
```

A clean, light, minimal chat page — white chat card, user messages on the right, Muse99 on the left, a slim top bar with the model/provider and the web address. Tabs: **Chat | Files | Tasks | Memory | Servers | Settings**. The address is also shown at the top of the web page itself. Settings has a provider dropdown, API base URL / model / key, web port, domain, and SSL on/off (port and SSL need a web UI restart from the menu).

## Give it a task — it works till the end

```bash
muse99 --task "Build a todo app in ./todo-app with tests" --allow-shell
```

It plans in `TASK.md`, builds, runs the build/tests, fixes failures, verifies by reading files back, and only then replies `DONE: …`. If it is blocked it says `BLOCKED: …` — it does not pretend.

From chat or the web Tasks tab you can also start a task, and use `spawn_subtask` to split off parallel pieces.

## Give it a server — only if you want to

Servers are opt-in. Add one in the web **Servers** tab, or in `~/.muse99/data/servers.json`:

```json
[{"name": "my-server", "host": "192.168.1.20", "port": 22, "user": "ubuntu", "enabled": true}]
```

Then ask it to run something there with the `remote_shell` tool. It uses your system `ssh` key. No server is used unless you add it and ask for it.

## Any API — providers

Muse99 keeps a key per provider, so switching back restores the right key. Add and switch providers in the SSH menu (option 9) or in web Settings — no need to edit config by hand. The old env vars still work as the first provider.

| Provider | Base URL | Default model |
|---|---|---|
| `spark` | https://api.meta.ai/v1 | muse-spark-1.3 |
| `openai` | https://api.openai.com/v1 | gpt-5 |
| `gemini` | https://generativelanguage.googleapis.com/v1beta/openai/ | gemini-2.5-flash |
| `openrouter` | https://openrouter.ai/api/v1 | openai/gpt-5 |
| `ollama` | http://localhost:11434/v1 | llama3.1 |
| `custom` | (you fill it in) | (you fill it in) |

Spark is Muse mode — the system prompt detects it by base URL / model and uses the Spark reasoning notes. The others use the same Muse99 persona with a different brain. Ollama needs no real key (the placeholder `ollama` is accepted) and runs locally.

```bash
# Or by hand, for the first provider only:
export MODEL_API_BASE_URL="https://api.meta.ai/v1"
export MODEL_ID="muse-spark-1.3"
export MODEL_API_KEY="your-key"
```

Config order: `~/.muse99/config.json` → `.env` → env vars → flags. The config file is `600` permissions — never commit it. Providers live in that same file, under `providers` / `active_provider`.

## Token usage — how much does a task cost?

Every API reply reports its tokens, and Muse99 adds them up in `~/.muse99/data/usage.json`. You can see them in three places:

- The SSH menu header (all-time totals), or menu option 11 (Show usage) for the full breakdown.
- The web UI header, and the Settings tab (total in/out, estimated cost, last task's tokens).
- At the end of every autonomous `--task` run: `Tokens: 1,234 in / 567 out, est. $0.0040`.

Cost is an estimate at Spark rates — $1.25 per 1M input tokens, $4.25 per 1M output tokens. Other providers are estimated at the same rates (their real prices differ, so treat it as a rough guide). Ollama runs locally, so its API cost is $0, but its tokens are still counted. If a provider does not report usage, that call is simply not counted.

## Domain and SSL

Point your domain at the server (an A record to its IP), open ports 80 and 443, then in the SSH menu: option 6 to set the domain, option 7 for SSL. If certbot is installed it runs `sudo certbot certonly --standalone -d <domain>` for you; if not, it prints the exact install commands. Certificates are looked for in `~/.muse99/certs/` and `/etc/letsencrypt/live/<domain>/`. Once SSL is on, restart the web UI (option 4) and the link on top of the menu becomes `https://your-domain`.

## Updating

In the SSH menu, option 8 (Update Muse99) downloads the latest release binary, checks it runs `--help`, then replaces the current binary. Quit and run `muse99` again to use it. Or just run the one-line installer again — it does the same thing.

## Tools (15)

`read_file`, `write_file`, `edit_file`, `list_files`, `search_files`, `run_shell`, `remote_shell`, `git_run`, `fetch_url`, `web_search`, `scaffold_app`, `spawn_subtask`, `get_time`, `remember`, `set_goal`

`scaffold_app` kinds: `go-cli`, `web-html`, `python-cli`, `node-express`

## Scheduler

```bash
# ~/.muse99/data/scheduled.json
[{"time": "08:00", "prompt": "Summarise my tasks and what is next"}]
muse99 --schedule
```

## Run it in production

```bash
make build          # vet + test + build
make web            # web UI on :8080
make docker         # docker build
docker compose up -d   # needs MODEL_API_KEY in .env

# systemd (edit the API key first)
cp muse99.service ~/.config/systemd/user/muse99.service
systemctl --user enable --now muse99
```

## Push to GitHub — sahfar363/muse99

```bash
git init
git add .
git commit -m "Muse99 — full production agent"
git branch -M main
git remote add origin https://github.com/sahfar363/muse99.git
git push -u origin main
```

GitHub Actions runs on every push/PR to `main` (`.github/workflows/ci.yml`: Ubuntu, Go 1.23, vet, test, build, smoke test). Push a tag and `release.yml` builds `muse99-linux-amd64` + `muse99-linux-arm64`:

```bash
git tag v0.1.0
git push origin v0.1.0
```

## Files

| File | What it is |
|---|---|
| `main.go` | CLI, config, `--task`, `--schedule`, `--web`, `--menu` |
| `agent.go` | Autonomous loop: plan → act → verify → DONE, 100 steps, sessions |
| `menu.go` | SSH menu — web address on top, start/stop/restart, port, domain, SSL, update, providers |
| `web.go` | Clean web dashboard — chat, files, tasks, memory, servers, settings; real start/stop/restart + TLS |
| `tools.go` | 15 tools incl. edit, search, git, remote SSH, scaffold, sub-agents |
| `client.go` | OpenAI-compatible `/chat/completions` client |
| `prompt.go` | Muse99 persona + Spark detection |
| `memory.go`, `store.go`, `config.go` | MEMORY.md, tasks/goals/servers/sessions, config + `.env`, provider presets |
| `scheduler.go` | Scheduled prompts |
| `Dockerfile`, `docker-compose.yml`, `Makefile` | Production packaging |
| `muse99.service` | systemd unit |
| `install.sh` | One-line Ubuntu installer — downloads the release binary, no Go, no compile |
| `.github/workflows/` | CI + Release |

## Safety

`--allow-shell` is off by default. Turn it on only in a directory you trust. Remote servers are opt-in only. `git_run` blocks `push --force`, `reset --hard`, `clean -f`. The agent asks before irreversible actions, but the shell tool runs what the model asks for — review `TASK.md` and `git diff` before you deploy anything it builds.

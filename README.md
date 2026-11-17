# Muse99 — a production agent like Muse

Give it a task and it works till the end. Give it a server only if you want it to — then it can work there, for free, on your own machine.

**Repo:** https://github.com/sahfar363/muse99

Muse99 is not the Muse app itself — that has hosted Gmail/Calendar, wallet, proactivity and Meta's runtime. Muse99 is the full, reproducible production core: same persona, memory, goals, autonomous task loop, sub-agents, tools, web dashboard, SSH menu, remote servers, scheduler, and packaging. Point it at Spark and it is Muse mode. Point it at any other OpenAI-compatible API and it keeps the same behaviour with a different brain.

## Quick install — Ubuntu server, one line

```bash
curl -fsSL https://raw.githubusercontent.com/sahfar363/muse99/main/install.sh | bash
```

That installs Go 1.23 if needed, clones/builds the repo, and puts `muse99` in `~/.local/bin/muse99`. Then:

```bash
export PATH="$HOME/.local/bin:$PATH"
export MODEL_API_KEY="your-key"
export MODEL_API_BASE_URL="https://api.meta.ai/v1"
export MODEL_ID="muse-spark-1.3"   # or muse-spark-1.4 when it ships
muse99 --allow-shell
```

## SSH menu — web address on top

Just run `muse99` over SSH:

```
═══════════════════════════════════════════════════════════
  ✦ Muse99  —  Web UI: http://192.168.1.10:8080
  Status: web UI is stopped  ·  choose 1 to start it
  Model: muse-spark-1.3  ·  Muse Spark — Muse mode
═══════════════════════════════════════════════════════════

  1  Start web UI
  2  Terminal chat
  3  Show web address / status
  4  Change API / model
  5  Show memory
  6  Quit
```

## Web UI — full dashboard

```bash
muse99 --web --port 8080 --allow-shell
# open http://your-server-ip:8080
```

Tabs: **Chat | Files | Tasks | Memory | Servers | Settings**
The address is also shown at the top of the web page itself.

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

## Any API

```bash
# Spark (Muse mode)
export MODEL_API_BASE_URL="https://api.meta.ai/v1"
export MODEL_ID="muse-spark-1.3"

# OpenAI / OpenRouter / Ollama — same agent, different brain
export MODEL_API_BASE_URL="https://api.openai.com/v1"
export MODEL_ID="gpt-5"
# export MODEL_API_BASE_URL="http://localhost:11434/v1"
# export MODEL_ID="llama3.1"
```

Config order: `~/.muse99/config.json` → `.env` → env vars → flags. The config file is `600` permissions — never commit it.

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
| `menu.go` | SSH menu — web address on top |
| `web.go` | Web dashboard — chat, files, tasks, memory, servers, settings |
| `tools.go` | 15 tools incl. edit, search, git, remote SSH, scaffold, sub-agents |
| `client.go` | OpenAI-compatible `/chat/completions` client |
| `prompt.go` | Muse99 persona + Spark detection |
| `memory.go`, `store.go`, `config.go` | MEMORY.md, tasks/goals/servers/sessions, config + `.env` |
| `scheduler.go` | Scheduled prompts |
| `Dockerfile`, `docker-compose.yml`, `Makefile` | Production packaging |
| `muse99.service` | systemd unit |
| `install.sh` | One-line Ubuntu installer |
| `.github/workflows/` | CI + Release |

## Safety

`--allow-shell` is off by default. Turn it on only in a directory you trust. Remote servers are opt-in only. `git_run` blocks `push --force`, `reset --hard`, `clean -f`. The agent asks before irreversible actions, but the shell tool runs what the model asks for — review `TASK.md` and `git diff` before you deploy anything it builds.

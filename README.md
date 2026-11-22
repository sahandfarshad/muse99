# Muse99 — a production agent like Muse

Give it a task and it works till the end. Give it a server only if you want it to — then it can work there, for free, on your own machine.

**Repo:** https://github.com/sahandfarshad/muse99

Muse99 is not the Muse app itself — that has hosted Gmail/Calendar, wallet, proactivity and Meta's runtime. Muse99 is the full, reproducible production core: same persona, memory, goals, autonomous task loop, sub-agents, tools, web dashboard, SSH menu, remote servers, scheduler, and packaging. Point it at Spark and it is Muse mode. Point it at any other OpenAI-compatible API and it keeps the same behaviour with a different brain.

## Quick install — Ubuntu server, one line, no compile

```bash
curl -fsSL https://raw.githubusercontent.com/sahandfarshad/muse99/main/install.sh | bash
```

That downloads the pre-built binary for your server (amd64 or arm64) from the latest GitHub release and puts it in `~/.local/bin/muse99`. No Go, no clone, no build on the server — GitHub Actions already built it when the release was tagged.

Then the installer does the rest by itself: it **starts the web UI** (default port **3759**) and **opens the SSH menu**, with the web UI link on top of the menu. No terminal setup is needed — the API key is set inside the web UI:

1. Open the web UI link from the top of the menu (e.g. `http://192.168.1.10:3759`).
2. Go to **Settings → Provider & model**, pick your provider, paste your API key.
3. Press **Fetch models**, choose a model, press **Test** to check it works, then **Save**.

That is the whole setup. Chat works right after that. (If you run the installer with no terminal attached — CI, a piped run — it starts the web UI in the background, prints the address, and exits instead of hanging. Run `muse99` later to open the menu. Advanced users can still pass a key for one run with `MODEL_API_KEY=your-key muse99 --menu=false`, but the web UI is the supported way.)

Specific version, or a different install folder:

```bash
curl -fsSL https://raw.githubusercontent.com/sahandfarshad/muse99/main/install.sh | bash -s -- --version v0.1.0
VERSION=v0.1.0 INSTALL_DIR=$HOME/bin bash install.sh
```

If no release exists yet, the installer stops with a clear error and tells you to create one (`git tag v0.1.0 && git push origin v0.1.0`) — it never silently falls back to compiling.

## Windows desktop app — local model (LM Studio, Ollama, …)

Download `Muse99.exe` from the latest release and double-click it. It opens Muse99 in its own window (Microsoft Edge app window), on `127.0.0.1` only, so nothing outside your computer can reach it, and no web login is needed. Close the window (or press **Quit** in the sidebar) to stop it. Your data lives in `%USERPROFILE%\.muse99`.

**LM Studio, automatically:** start LM Studio's server (Developer tab → Start Server, port 1234) with a model such as OxCoder-9B. When no model is set up yet, Muse99 connects to it by itself at start, picking the loaded model first.

**Any other local program:** Settings → **Local model** → *Find local servers* finds LM Studio (1234), Ollama (11434), llama.cpp / LocalAI (8080), Jan (1337), vLLM (8000) and KoboldCpp (5001) and lists their models; click one to use it. For anything else type its address (e.g. `127.0.0.1:5000`) and press *Check address*, or pick a provider in **Provider & model** and enter the base URL and model by hand.

**Your own folder:** Settings → **Work folder** → *Choose folder…* (or type the path) → *Use this folder*. Muse99 reads, creates and edits files only inside that folder, and shell commands run there. It is remembered; the default is `%USERPROFILE%\Muse99`. Memory stays in `%USERPROFILE%\.muse99`.

**Internet search:** built in, nothing to set up. `web_search` returns clean results (DuckDuckGo, Bing as fallback) and `fetch_url` reads a page. The model must support tool calls (OxCoder, Qwen 3 and most modern models do). For more reliable search set a free Brave Search API key as `MUSE_BRAVE_KEY` (in `%USERPROFILE%\.muse99\.env`).

On Windows the shell tool uses PowerShell. `muse99-cli.exe` is the same program with a console, for the terminal chat and flags. Tip: give the model a long context in LM Studio (32K or more) — agents need it.

## SSH menu — web address on top

Just run `muse99` over SSH:

```
═══════════════════════════════════════════════════════════
  ✦ Muse99 v0.8.0  —  Web UI: http://192.168.1.10:3759
  Status: web UI is RUNNING  ·  open the address above in your browser
  Provider: spark  ·  Model: muse-spark-1.3  ·  Muse Spark — Muse mode
  User: Sahand  ·  Workdir: /home/ubuntu
═══════════════════════════════════════════════════════════

  1  Start web UI
  2  Terminal chat
  3  Stop web UI
  4  Restart web UI
  5  Choose web port
  6  Set domain
  7  Set up SSL / Let's Encrypt
  8  Update / roll back (now v0.8.0)
  9  Providers / API keys
  10 Show memory
  11 Show usage
  12 Show web address / status
  13 Chats
  14 Toggle shell commands
  15 Web login (username / password)
  17 Your name
  16 Uninstall Muse99
  0  Exit
```

The version you run is always on the top line (and next to the logo in the web UI; `muse99 --version` prints it). The first time you open the menu or the web UI, Muse99 asks what to call you and uses that name from then on; change it with option 17 or in web Settings → Your name.

**0 is the exit**, and 0 also cancels out of any prompt inside the menu (a port, a domain, a password) instead of being taken as the value. The web UI started from the menu runs as its own process, so it keeps serving after you quit the menu.

Everything the server needs is in the menu: start / stop / restart the web UI, change its port, set a domain, set up SSL with Let's Encrypt (it checks for certbot and for existing certificates first), update Muse99 itself to the latest release, and switch API providers. The web link on top shows `https://your-domain` once a domain and SSL are set, otherwise `http://<server-ip>:<port>`. Option 14 toggles shell commands (same setting as the web UI checkbox), and the menu header shows whether shell is ON or OFF. Inside Providers (option 9), **l** lists the current provider's models and lets you pick one by number, and **t** tests a model to see if it answers.

If the menu does not open after installing, it is almost always one of two things: (1) `muse99` is not on your PATH yet on a fresh server — run it by its full path, `~/.local/bin/muse99`, or add `export PATH="$HOME/.local/bin:$PATH"` to `~/.bashrc`; (2) the install ran without a terminal (a web panel, or `ssh host 'curl … | bash'`), so there was no screen to open the menu on — just run `muse99` (or its full path) in a normal SSH session. The installer prints the exact command to run.

## Chats — side chats that survive a restart

Chats are stored in a small database (`~/.muse99/data/muse99.db`, see below), not in memory only. Start a new chat, switch between them, rename, delete, or search by title or text — in the web UI sidebar, or in terminal chat with `/chats`, `/new [title]`, `/open <number or id>`. On restart, every chat and its messages reload. The first user message becomes the chat title automatically.

## Memory — advanced, like Muse's

Each durable fact is a **memory** with text, kind (preference / fact / decision / project / person), source chat, date, and confidence, stored in the same database. When a turn starts, Muse99 scores memories by keyword overlap with your message plus recency, and puts the top ~10 (max ~4000 chars) into the system prompt — so it remembers what matters for *this* question, not just the last lines of a file.

How memories get saved:

- The `remember` tool: the model saves a fact explicitly when you ask it to ("remember that I prefer Go").
- **Auto-summary**: when a chat reaches 10 new messages since its last summary, Muse99 asks the model once to extract durable facts only (preferences, decisions, projects, names, constraints), dedupes them, and saves the new ones. It does **not** call the model on every turn — that would roughly double your token cost. You can also summarise on demand: "Summarise this chat" in the web Memory tab, or `/summarise` in terminal chat.
- The Memory tab lists every memory with kind and date — edit or delete one there to correct or forget it.

`MEMORY.md` stays in sync: it is exported from the database (grouped by kind) whenever a memory is saved, and an existing `MEMORY.md` is imported once on first run if the database is empty. Importing never rewrites your file. Token usage totals are also kept in the database (migrated from `usage.json` on first run); `usage.json` is still written as a fallback export.

## Web login — username and password

The web UI can run commands and holds your API keys, so it is **never served without a login**. Set the login in the SSH menu, option **15 (Web login)**: pick a username and a password (at least 8 characters, typed without echo). Only a bcrypt hash is saved in `~/.muse99/config.json`. Until a login is set, the web UI shows a locked page that says how to set one, and the menu header shows `Web login: NOT SET`.

- Sessions are an HttpOnly, SameSite=Strict cookie (Secure when SSL is on), valid for 7 days; restarting the web UI signs everyone out. **Log out** is in the top bar.
- 5 wrong passwords from one IP lock that IP out for 15 minutes.
- Requests from another site are refused (Origin check), so a malicious page cannot drive your Muse99 through your browser.
- Docker / systemd: set `MUSE99_WEB_USER` and `MUSE99_WEB_PASSWORD` in the environment instead; they override the saved login.

### Timeouts

- Each shell command is killed after 120 s (`MUSE99_SHELL_TIMEOUT`, seconds).
- Each model call may take up to 15 min, for reasoning models that think long (`MUSE99_MODEL_TIMEOUT`, seconds).
- Each reply may use up to 8192 tokens, thinking included (`MUSE99_MAX_TOKENS`). If a reasoning model spends it all thinking, Muse99 says so instead of showing an empty reply.
- `/api/status` answers without a login only with `{"ok":true}`, so the menu and installer can still see the UI is up.
- The Files tab only opens paths inside the workdir.

## Shell — a checkbox, not a flag

Shell commands are **off by default**. Turn them on with the **Allow shell commands** checkbox in the web UI top bar or in Settings, or with SSH menu option 14, or in terminal chat with `/shell`. The warning next to it says it plainly: *"Lets Muse99 run commands on this server. Only turn on in a folder you trust."* Toggling takes effect immediately — no restart — and is saved to `~/.muse99/config.json` (`allow_shell`), so it survives restarts. The old `--allow-shell` flag still works and sets the same setting for that run.

## Browser — check anything

Ask Muse99 to check a page and it uses the `browse_url` tool (`fetch_url` is the same thing, kept for compatibility). Two layers:

- **Layer 1 (default, always on, tiny RAM):** an HTTP fetch with a browser User-Agent, redirects, gzip, 20-second timeout, then the HTML is parsed to clean text (title, headings, main text, links). Covers most docs, blogs, GitHub pages, and static price pages.
- **Layer 2 (opt-in, JavaScript pages only):** headless Chromium, if it is installed on the server, rendering the page and parsing the DOM the same way. It is **not bundled, not auto-installed, and not run by default** — a Chromium page uses **~300–500 MB RAM**, which is a lot on a 1 GB server. If Chromium is missing, the tool says so and tells you how to install it (`sudo apt update && sudo apt install -y chromium-browser`). Use it sparingly, only when Layer 1 returns nothing useful (e.g. a page that says "enable JavaScript").

## Running on 1 GB

Muse99 is built for a small server — a 1 GB RAM Ubuntu 22.04 box is fine:

- **Binary:** ~10 MB. No Go, no compile, no database server on the box.
- **RAM when idle:** ~10 MB RSS for the Go process (measured: 9,280 KB with the web UI running). The model runs in the cloud, so there is no model RAM at all — the only thing that costs RAM locally is what the agent itself does.
- **Database:** one embedded file (`~/.muse99/data/muse99.db`, bbolt — a pure-Go key/value store, no CGO, no daemon). It is memory-mapped, so its RSS roughly follows the file size: tens of KB for normal use. Only the last 100 messages of a chat are held in RAM; the rest stays in the database.
- **Swap:** if you run anything else heavy on the same box, a 1 GB swap file is a cheap safety net: `sudo fallocate -l 1G /swapfile && sudo chmod 600 /swapfile && sudo mkswap /swapfile && sudo swapon /swapfile`.
- **If memory gets tight:** do not use headless Chromium (Layer 2 browsing, ~300–500 MB per page); keep `web_search` queries small; close chats you no longer need (deleting a chat frees its messages from the database).

Why bbolt and not SQLite: the current SQLite driver (modernc.org/sqlite v1.62) needs the Go 1.26 toolchain, while this project and its CI pin Go 1.23, and it would add ~10 MB to the binary. bbolt builds on Go 1.23 and adds ~1 MB. If a future SQLite driver supports Go 1.23 again, the database layer is one file (`db.go`) and can be swapped.

## Models — fetch, click to test, set active

You never type a model name. In web **Settings → Provider & model**, choose a provider, paste its API key, press **Fetch models** — and the provider's own list appears as clickable rows. Click a row (or its **Test** button) and Muse99 sends one tiny message ("Reply with the single word OK") to that model and shows, right on the row, whether it answers, how fast, and what it replied. Press **Set active** on the one you want and that is the model Muse99 chats with from then on — the row gets an ACTIVE badge and the header updates at once. **Test all** tests the first 20 models one by one, so you can see at a glance which ones work. Type in the filter box to narrow a long list.

The only typing left is a fallback that stays hidden until you need it: if Fetch returns no models at all, a link appears — "Fetch didn't return any models? Enter one manually" — and only then can you type a name.

How it works: Muse99 calls the provider's `GET {base_url}/models` endpoint with your key (15 s timeout). The list is cached for 10 minutes and the last-known list is saved in the database, so the chooser still works offline after a restart — it says whether the list is live, cached, or suggested, and when it was fetched. Opening Settings only ever shows the saved list; it never fetches live on its own, so a slow or unreachable provider can't make the Settings page hang — press **Fetch models** when you want a fresh list. Your API key is never shown, logged, or returned by any of these endpoints.

Provider quirks:

- **Spark, OpenAI, OpenRouter:** `/v1/models` works with the key.
- **Gemini:** the OpenAI-compatible list endpoint is flaky — it can come back empty, or refuse a key that works fine elsewhere. Muse99 now works around that three ways: (1) on a 401/403 it retries with Gemini's native `x-goog-api-key` header instead of Bearer, for the list *and* for chat; (2) if the compatible list is empty or errors, it asks Gemini's native `GET /v1beta/models` list instead; (3) Gemini's native list returns ids as `models/gemini-2.5-flash`, and that `models/` prefix is stripped everywhere — list, test, save, and chat — because chat completions need the bare id `gemini-2.5-flash` (sending the prefixed id fails with "model not found"). Only if all of that fails does Muse99 show suggested models (`gemini-2.5-flash`, `gemini-2.5-pro`, `gemini-2.0-flash`). If Gemini still won't connect, check: the key comes from [Google AI Studio](https://aistudio.google.com/apikey) (a plain API key, not an OAuth token), the base URL is exactly `https://generativelanguage.googleapis.com/v1beta/openai/`, and the key is pasted in Settings and saved — the Settings page now also says whether the key in use came from config.json ("saved") or from the environment ("env"; pressing Save settings keeps it in config.json).
- **Ollama:** `/v1/models` works; if it fails, Muse99 falls back to Ollama's native `GET /api/tags` and maps tag names to model ids. No key needed.

The same thing is in the SSH menu, Providers (option 9): **l** lists the current provider's models and lets you pick one by number, **t** tests a model. API endpoints, for scripts: `GET /api/models?provider=spark`, `POST /api/models/refresh {provider, base_url, api_key}`, `POST /api/models/test {provider|base_url, api_key, model}` → `{ok, model, latency_ms, reply, error}`, `POST /api/models/active {provider|base_url, api_key, model}` → `{ok, model, provider}`.

## Behind the scenes — see every step

Press **◧ Behind the scenes** at the top of the chat. A panel opens on the right and shows, live, what Muse99 is doing: waiting for the model (with a running timer), the model's thinking when the provider sends it, which tool it wants, the exact command and its output, retries, and errors. While you wait, the "working" line under the chat also says the current step, so you can tell "still thinking" from "stuck". The panel keeps the last 400 steps in memory only; keys and tokens are hidden.

Settings: **Save settings** is pinned at the top, and saving shows a short note that fades away.

Marketplace providers (AiPrime, OpenRouter) price a request by its worst case. When one answers "No seller can serve this model at an affordable price" for the reply budget, Muse99 retries with 2048, then 512, then no limit, and keeps what worked. Model tests use a small budget.

When the connection drops or the provider is busy (connection reset, timeout, 429, 5xx, "no seller right now"), Muse99 waits and retries: 5 s, 15 s, 30 s, 1 min, 2 min. You see the countdown in Behind the scenes. If it still fails, the steps and files so far are kept; send "continue". Old tool outputs are shortened in what is sent (the chat keeps them whole), so long tasks cost fewer tokens.

## Database tab and per-chat Behind the scenes

Each chat keeps its own **Behind the scenes** history, saved in the database (last 500 steps per chat), so it is still there after a restart and switching chats shows that chat's steps. **Clear** in the panel clears only the open chat.

The **Database** tab lists what `muse99.db` holds: chats, messages, memories, Behind the scenes history, usage totals and saved model lists, with counts and sizes. Open a section, search, click an entry to read it, and delete what you don't want. Deleting a chat also deletes its messages and its history.

## Pace (free models with a per-minute limit)

The **Pace** menu at the top of the chat makes Muse99 wait 1 to 60 seconds between one model call ending and the next starting. Free models such as OpenRouter's `:free` ones allow only a few requests a minute; 3 to 10 s keeps a long task from hitting that limit. Saved in `config.json` (`call_delay_sec`), or set `MUSE99_CALL_DELAY=5`. If a 429 still happens, the normal retry waits take over.

Persian and Arabic replies are laid out right-to-left line by line, so a reply that starts with an English word (a name, a command) no longer flips the whole bubble; code stays left-to-right.

## Your servers (SSH)

In the **Servers** tab add a name, user, host, port and, if you like, the password. Then just tell Muse99 "on myserver, check the disk". It connects with its own SSH client (no system ssh needed, and the local Shell switch is not needed for this), trusts a server's host key the first time and refuses if it ever changes, and only uses servers saved in this list.

Safer than a password: the tab shows Muse99's own key as one ready command with a **Copy** button. Log in to the new server once, paste that line, done; nothing to copy back. A server with a saved password also gets an **Install key** button that does it for you. **Test connection** shows who it logged in as. Passwords stay in `~/.muse99/servers.json` (readable only by you) and are never sent back to the browser.

## Switch model mid-chat, and a backup model

The model menu at the top of the chat lists the models you used recently. Pick another one at any time: the chat, its steps and files stay, and the next message goes to the new model (send "continue" to carry on). Next to it, **Backup** picks a model that takes over by itself when the main one still fails after its retries; the reply then says it finished with the backup.

## Web UI — minimal, slick, modern

```bash
muse99 --web --port 3759
# open http://your-server-ip:3759
```

One page, no external CSS or JavaScript, no build step — it works offline on a 1 GB server. Restrained light palette with one indigo accent, rounded cards, soft shadows, pill badges for provider/model, a slim collapsible chat sidebar, and a focused chat area with the composer pinned at the bottom. Tabs: **Chat | Files | Tasks | Memory | Servers | Settings**. The address is also shown at the top of the web page itself. The Chat tab has a sidebar with your chats (New chat, switch, rename, delete, search). Settings has the provider & model picker (above), web port, domain, SSL on/off, and the **Allow shell commands** checkbox (port and SSL need a web UI restart from the menu).

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

Muse99 keeps a key per provider, so switching back restores the right key — and its fetched model list, so you don't fetch again. A key is saved under the provider's name as soon as Fetch models works with it. You can also add your own named providers (Settings → Provider → **+ Add a provider**, e.g. `glm`, `deepseek`), each with its own base URL, key and model. Add and switch providers in the SSH menu (option 9) or in web Settings — no need to edit config by hand. The old env vars still work as the first provider.

| Provider | Base URL | Default model |
|---|---|---|
| `spark` | https://api.meta.ai/v1 | muse-spark-1.3 |
| `openai` | https://api.openai.com/v1 | gpt-5 |
| `gemini` | https://generativelanguage.googleapis.com/v1beta/openai/ | gemini-2.5-flash |
| `openrouter` | https://openrouter.ai/api/v1 | openai/gpt-5 |
| `ollama` | http://localhost:11434/v1 | llama3.1 |
| `custom` | (you fill it in) | (you fill it in) |

Spark is Muse mode — the system prompt detects it by base URL / model and uses the Spark reasoning notes. The others use the same Muse99 persona with a different brain. Ollama needs no real key (the placeholder `ollama` is accepted) and runs locally.

(Adding a provider by hand in a terminal is not needed any more: use **Settings → Provider & model** in the web UI, or the SSH menu option 9. Advanced users can still pass `MODEL_API_BASE_URL` / `MODEL_ID` / `MODEL_API_KEY` as env vars for a single run.)

Config order: `~/.muse99/config.json` → `.env` → env vars → flags. The config file is `600` permissions — never commit it. Providers live in that same file, under `providers` / `active_provider`.

## Token usage — how much does a task cost?

Every API reply reports its tokens, and Muse99 adds them up in `~/.muse99/data/usage.json`. You can see them in three places:

- The SSH menu header (all-time totals), or menu option 11 (Show usage) for the full breakdown.
- Under every chat reply: the tokens that reply used and the model that answered (e.g. `1,234 tokens (1,100 in / 134 out) · glm-5.3-flash`).
- The web UI header (total + last model), and the Settings tab: totals plus a table of tokens **per model**.
- Menu option 11 shows the same per-model table. (Per-model counts start with v0.8; older calls are only in the totals.)
- At the end of every autonomous `--task` run: `Tokens: 1,234 in / 567 out, est. $0.0040`.

Cost is an estimate at Spark rates — $1.25 per 1M input tokens, $4.25 per 1M output tokens. Other providers are estimated at the same rates (their real prices differ, so treat it as a rough guide). Ollama runs locally, so its API cost is $0, but its tokens are still counted. If a provider does not report usage, that call is simply not counted.

## Domain and SSL

Point your domain at the server (an A record to its IP), open port 80 (for the Let's Encrypt check) and the web UI port, then in the SSH menu: option 6 to set the domain, option 7 for SSL. If certbot is installed it runs `sudo certbot certonly --standalone -d <domain>` for you; if not, it prints the exact install commands.

The Let's Encrypt private key is root-only, so Muse99 copies the pair into `~/.muse99/certs/` (key `0600`, owned by you) and installs a certbot deploy hook (`/etc/letsencrypt/renewal-hooks/deploy/muse99.sh`) that repeats the copy after every renewal. The web UI re-reads the certificate when the file changes, so renewals apply **without a restart**. Once SSL is on, restart the web UI (option 4); the link on top of the menu becomes `https://your-domain:<port>` (or `https://your-domain` when the port is 443).

## Updating

In the SSH menu, option 8 (Update / roll back) shows the version you are on and the latest release, and offers: **1** update to the latest, **2** roll back to the previous version, **3** install any version by tag (e.g. `v0.7.2`). Every update keeps the binary it replaces as `muse99.prev`, so roll back is one step (and rolling back again returns to the newer one). It downloads the release binary, checks it runs `--help`, then replaces the current binary — and **restarts the web UI from the new binary**, so the update actually takes effect. Or just run the one-line installer again — it does the same thing, and also restarts the web UI if it was already running. (The menu itself is still the old version until you quit with 0 and run `muse99` again.)

## Uninstalling

SSH menu option **16 (Uninstall Muse99)**, or `muse99 --uninstall`, or `bash uninstall.sh`. It stops the web UI first, shows exactly what it will remove, and only proceeds after you type **YES**. It removes the binary and any systemd service file, and removes the install folder only if it is empty. Your data in `~/.muse99` (config, chats, memory, usage) is **kept by default** — it is only deleted if you answer yes to the data question, or pass `--yes` (`muse99 --uninstall --yes` is fully non-interactive and removes data too).

## Tools (16)

`read_file`, `write_file`, `edit_file`, `list_files`, `search_files`, `run_shell`, `remote_shell`, `git_run`, `fetch_url`, `browse_url`, `web_search`, `scaffold_app`, `spawn_subtask`, `get_time`, `remember`, `set_goal`

`browse_url` fetches a page as clean text (title + headings + main text + links); set `js: true` only for JavaScript pages, which uses headless Chromium if installed (~300–500 MB RAM). `fetch_url` is the same fetch, kept for compatibility.

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
make web            # web UI on :3759
make docker         # docker build
docker compose up -d   # then set the key in the web UI, or put MODEL_API_KEY in .env first

# systemd (no API key in the file — set it in the web UI, Settings → Provider & model)
cp muse99.service ~/.config/systemd/user/muse99.service
systemctl --user enable --now muse99
```

## Push to GitHub — sahandfarshad/muse99

```bash
git init
git add .
git commit -m "Muse99 — full production agent"
git branch -M main
git remote add origin https://github.com/sahandfarshad/muse99.git
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
| `menu.go` | SSH menu — web address on top, start/stop/restart, port, domain, SSL, update, providers, chats, shell toggle |
| `auth.go` | Web login — bcrypt password, session cookie, rate limit, same-origin check, SSH menu option 15 |
| `certs.go` | TLS — certificate hot reload, Let's Encrypt copy + renewal hook, workdir path check |
| `web.go` | Modern minimal web dashboard — chat sidebar, files, tasks, memory, servers, settings; model picker; real start/stop/restart + TLS |
| `models.go` | Model discovery + testing — `GET /models` fetch, 10-min cache, persisted last-known list, `/api/models` + `/api/models/test` |
| `db.go` | Embedded database (bbolt) — persistent chats, memories, usage, model lists; scoring, dedupe, auto-summary |
| `browse.go` | Browser layers: HTTP fetch + HTML-to-text, optional headless Chromium |
| `tools.go` | 16 tools incl. edit, search, git, remote SSH, scaffold, sub-agents, browse_url |
| `client.go` | OpenAI-compatible `/chat/completions` client |
| `prompt.go` | Muse99 persona + Spark detection |
| `memory.go`, `store.go`, `config.go` | MEMORY.md, tasks/goals/servers/sessions, config + `.env`, provider presets |
| `usage.go` | Token usage tracking (also kept in the database) |
| `scheduler.go` | Scheduled prompts |
| `Dockerfile`, `docker-compose.yml`, `Makefile` | Production packaging |
| `muse99.service` | systemd unit |
| `install.sh` | One-line Ubuntu installer — downloads the release binary, starts the web UI (port 3759), opens the SSH menu |
| `uninstall.go`, `uninstall.sh` | Uninstall — menu option 16, `muse99 --uninstall`, or `bash uninstall.sh`; data kept unless you say yes |
| `.github/workflows/` | CI + Release |

## Safety

Shell commands are off by default — turn them on only in a directory you trust, with the web UI checkbox or SSH menu option 14 (`--allow-shell` does the same for one run). Remote servers are opt-in only. `git_run` blocks `push --force`, `reset --hard`, `clean -f`. The agent asks before irreversible actions, but the shell tool runs what the model asks for — review `TASK.md` and `git diff` before you deploy anything it builds.

# Changelog

## v1.1.0-beta.6 (2026-02-28)

### New Features
- **QQ Platform** (Beta): Support QQ messaging via OneBot v11 / NapCat WebSocket
- **Cron Scheduling**: Schedule recurring tasks via `/cron` command or CLI (`cc-connect cron add`), with JSON persistence and agent-aware session injection
- **Feishu Emoji Reaction**: Auto-add emoji reaction (default: "OnIt") on incoming messages to confirm receipt; configurable via `reaction_emoji`
- **Display Truncation Config**: New `[display]` config section to control thinking/tool message truncation (`thinking_max_len`, `tool_max_len`); set to 0 to disable truncation
- **`/version` Command**: Check current cc-connect version from within chat

### Bug Fixes
- **Windows `/list` fix**: Claude Code sessions now discoverable on Windows despite drive letter colon in project key paths
- **CLAUDECODE env filter**: Prevent nested Claude Code session crash by filtering CLAUDECODE env var from subprocesses

### Docs
- Clarified global config path `~/.cc-connect/config.toml` in INSTALL.md
- Fixed markdown image syntax in Chinese README

## v1.1.0-beta.5 (2026-03-01)

### New Features
- **Gemini CLI Agent**: Full support for `gemini` CLI with streaming JSON, mode switching, and provider management
- **Cursor Agent**: Integration with Cursor Agent CLI (`agent`) with mode and provider support

## v1.1.0-beta.4 (2026-03-01)

### Bug Fixes
- Fixed npm install: check binary version on install, replace outdated binary instead of skipping
- Added auto-reinstall logic for outdated binaries in `run.js`

## v1.1.0-beta.3 (2026-03-01)

### New Features
- **Voice Messages (STT)**: Transcribe voice messages to text via OpenAI Whisper, Groq Whisper, or SiliconFlow SenseVoice; requires `ffmpeg`
- **Image Support**: Handle image messages across platforms with multimodal content forwarding to agents
- **CLI Send**: `cc-connect send` command and internal Unix socket API for programmatic message sending
- **Message Dedup**: Prevent duplicate processing of WeChat Work messages

## v1.1.0-beta.2 (2026-03-01)

### New Features
- **Provider Management**: `/provider` command for runtime API provider switching; CLI `cc-connect provider add/list`
- **Configurable Data Dir**: Session data stored in `~/.cc-connect/` by default (configurable via `data_dir`)
- **Markdown Stripping**: Plain text fallback for platforms that don't support markdown (e.g. WeChat)

## v1.1.0-beta.1 (2026-03-01)

### New Features
- **Codex Agent**: OpenAI Codex CLI integration
- **Self-Update**: `cc-connect update` and `cc-connect check-update` commands
- **I18n**: Auto-detect language, `/lang` command to switch between English and Chinese
- **Session Persistence**: Sessions saved to disk as JSON, restored on restart

## v1.0.1 (2026-02-28)

- Bug fixes and stability improvements

## v1.0.0 (2026-02-28)

- Initial release
- Claude Code agent support
- Platforms: Feishu, DingTalk, Telegram, Slack, Discord, LINE, WeChat Work
- Commands: `/new`, `/list`, `/switch`, `/history`, `/quiet`, `/mode`, `/allow`, `/stop`, `/help`

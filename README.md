# claude-bouncer

`claude-bouncer` is a Claude Code [`PreToolUse` hook](https://code.claude.com/docs/en/hooks) that keeps quota for one session, the **patron**. During an exclusive window on each weekday it holds every other session's tool calls until the window closes. Outside the window it makes sure enough of the weekly limit stays unspent for the patron's remaining windows before the weekly reset. If too little would be left, it holds every session, the patron included, until the next window closes.

It only reads files and sleeps. It never writes anything.

## Why

Say a Claude Code session trades from 8:30 to 13:30 on weekdays and needs about 10% of the weekly limit each time. After the close it keeps working on research, and other sessions on other machines share the same account. Without a guard, any of them can use up the week before the patron's next trading window, including the patron itself.

## How it works

The hook runs before every tool call of every session where it is installed:

1. **Inside the exclusive window** (for example 8:30 to 13:30, Monday to Friday), the patron's calls run at once. Every other session's call sleeps until the window closes.
2. **Outside the window**, including all of Saturday and Sunday, every session's call runs while

   ```text
   weekly limit left  >  daily_reserve  x  windows left before the weekly reset
   ```

   Otherwise the call sleeps until the next window closes. The patron sleeps only until the next window opens, where rule 1 lets it run again.

Holding until the close, instead of failing the call, matters. The patron may need less than its share on a given day, so after the close the numbers are worked out again. Whatever the patron left unused becomes available to everyone.

Windows exist on weekdays only, so weekends add nothing to the reserve. After Friday's close, the next window is Monday's. A window still counts toward the reserve until it closes, and a window that is open when the weekly reset happens counts in full. A window that runs past midnight belongs to the weekday it starts on.

While a call is held, `claude-bouncer` reads the policy and the usage file again every minute, so editing the policy takes effect at once. When a held call is released, the hook tells the user and Claude how long it was held, so a session that resumes hours later knows that time has passed.

### Where the weekly figure comes from

Anthropic offers no API for the remaining quota, but every Claude Code status line sees it. [`continue-claude`](https://github.com/caridina-ai/continue-claude) publishes those figures to a shared file with `-export`:

```json
{
  "statusLine": {
    "type": "command",
    "command": "continue-claude -export X:/claude_usage.json"
  }
}
```

`claude-bouncer` reads `rate_limits.seven_day` from that file. If the file's `resets_at` has already passed, the week has reset and its usage is taken as 0%.

Put the usage file and the policy file on a drive that every machine mounts at the same path, such as a NAS share mapped to `X:`. Then one policy covers every machine.

## Install

```powershell
go install github.com/caridina-ai/claude-bouncer/cmd/claude-bouncer@latest
```

This puts `claude-bouncer` in Go's bin directory (`$(go env GOPATH)\bin`), which should be on `PATH`. You also need a status line that publishes the usage file, such as `continue-claude -export` above.

## Policy file

The policy is a JSON file with exactly four fields:

```json
{
  "patron": "trader",
  "start": "8:30",
  "end": "13:30",
  "daily_reserve": 10
}
```

| Field | Meaning |
| --- | --- |
| `patron` | The patron's session name, compared case-insensitively. A session ID works too. |
| `start` | When the exclusive window opens each weekday, `H:MM` in local time. |
| `end` | When it closes. A time earlier than `start` means the window runs past midnight. |
| `daily_reserve` | Percent of the weekly limit kept for the patron for each window left before the weekly reset. |

Unknown fields are rejected, so a typo cannot silently disable a rule.

### Naming the patron

Start the patron with a name, or rename it inside the session:

```powershell
claude --name trader
```

```text
/rename trader
```

The `PreToolUse` input carries a session ID but no name, so `claude-bouncer` looks the name up in Claude Code's session registry (`~/.claude/sessions/<pid>.json`). If the session is not there, it uses the last custom title in the session's transcript.

## Claude Code configuration

Add the hook to `~/.claude/settings.json` on every machine:

```json
{
  "hooks": {
    "PreToolUse": [
      {
        "matcher": "*",
        "hooks": [
          {
            "type": "command",
            "command": "claude-bouncer -config X:/claude-bouncer.json -usage X:/claude_usage.json",
            "timeout": 604800,
            "statusMessage": "claude-bouncer"
          }
        ]
      }
    ]
  }
}
```

- **Set a long `timeout`.** Claude Code cancels a hook after 600 seconds by default, and on `PreToolUse` a cancelled hook lets the tool call go ahead. A held call can sleep for a whole day, or longer if the reserve stays short. `604800` is seven days.
- **Put it first.** If you already have `PreToolUse` hooks, add this one at the front of the `hooks` array for each matcher, so a held call waits before your other hooks run.
- **Use forward slashes** in Windows paths. They work in Windows paths, and a shell that treats `\` as an escape leaves them alone.

To release a held call by hand, press `Esc` in that session, or edit the policy file.

## Check a decision

`-dry-run` prints what the hook would decide, without sleeping:

```powershell
claude-bouncer -config X:/claude-bouncer.json -usage X:/claude_usage.json -dry-run -session research
```

```text
now       2026-11-03 14:00
window    outside 8:30-13:30 Mon-Fri, next opens Wed 11/4 8:30
session   research, patron "trader": no
weekly    used 85.0%, left 15.0%, resets 11/5 15:00
reserve   10.0% x 2 window(s) before reset = 20.0%
decision  hold until 11/4 13:30: weekly reserve for "trader": 15.0% left, 10.0% x 2 window(s) = 20.0% kept until reset 11/5 15:00
```

Add `-now "2026-11-04 09:00"` to see what would happen at another time.

## Options

| Option | Default | Meaning |
| --- | --- | --- |
| `-config FILE` | required | The policy file. |
| `-usage FILE` | required | The usage file published by `continue-claude -export`. |
| `-poll DURATION` | `1m` | How often a held call reads both files again. |
| `-dry-run` | off | Print the decision instead of sleeping. |
| `-session NAME` | | With `-dry-run`: decide for this session name instead of reading hook JSON from stdin. |
| `-now "YYYY-MM-DD HH:MM"` | | With `-dry-run`: decide at this local time. |
| `-version` | | Print the version. |

Both file paths are required, with no defaults, so the settings file always shows which files are in use.

## Failure behavior

The hook never blocks a tool call outright. It delays it, or it lets it run:

- It never exits with code 2, which would make Claude Code reject the call.
- If the policy file cannot be read, or the usage file cannot be read outside the window, the call runs. The hook exits with code 1, which Claude Code shows as a hook error notice, so the problem is visible.
- If the files become unreadable while a call is held, for example because the share drops, the hold continues as planned and ends at its scheduled time.
- If the Claude Code process exits while a call is held, the hook exits too.

## AI provenance

Written by Claude Code using Claude Opus 5.5.

# claude-bouncer

`claude-bouncer` is a Claude Code [`PreToolUse` hook](https://code.claude.com/docs/en/hooks) that keeps quota for one session, the **patron**. During an exclusive window on each weekday it holds every other session's tool calls until the window closes. Outside the window it makes sure enough of the weekly limit stays unspent for the patron's remaining windows before the weekly reset. If too little would be left, it holds every session, the patron included, until the next window closes. Around the window it also watches the five-hour limit, so that the patron is not rate-limited when it needs to work.

It only reads files and sleeps. It never writes anything.

## Why

Say a Claude Code session trades from 8:30 to 13:30 on weekdays and needs about 10% of the weekly limit each time. After the close it keeps working on research, and other sessions on other machines share the same account. Without a guard, any of them can use up the week before the patron's next trading window, including the patron itself. The five-hour limit has the same problem on a smaller scale: research at 7:00 starts a block that runs until noon, and if that block is used up before 8:30, Claude stops every session, the patron included, until it resets.

## How it works

The hook runs before every tool call of every session where it is installed:

1. **Inside the exclusive window** (for example 8:30 to 13:30, Monday to Friday), the patron's calls run at once. Every other session's call sleeps until the window closes.
2. **Outside the window**, including all of Saturday and Sunday, every session's call runs while

   ```text
   weekly limit left  >  daily_reserve  x  windows left before the weekly reset
   ```

   Otherwise the call sleeps until the next window closes. The patron sleeps only until the next window opens, where rule 1 lets it run again.
3. **From five hours before the window opens until it closes**, the five-hour limit is watched. Once its usage reaches `five_hour_threshold` (95% unless set), every other session's call sleeps until the window closes. The patron is never held for it. Instead, each of its calls carries a warning to slow down. See [The five-hour guard](#the-five-hour-guard).

Holding until the close, instead of failing the call, matters. The patron may need less than its share on a given day, so after the close the numbers are worked out again. Whatever the patron left unused becomes available to everyone.

Windows exist on weekdays only, so weekends add nothing to the reserve. After Friday's close, the next window is Monday's. A window still counts toward the reserve until it closes, and a window that is open when the weekly reset happens counts in full. A window that runs past midnight belongs to the weekday it starts on.

While a call is held, `claude-bouncer` reads the policy and the usage file again every minute, so editing the policy takes effect at once. When a held call is released, the hook tells the user and Claude how long it was held, so a session that resumes hours later knows that time has passed.

### The five-hour guard

A five-hour block starts with the first request after the previous block ended and resets five hours later. When the account reaches 100% of a block, Claude stops every session until the block resets, the patron included. Any block started in the five hours before the open is still running when the window opens, so that is when the guard starts. It ends at the close.

The idea is that being held by the bouncer is better than being stopped by Claude:

- **Other sessions** are held as soon as usage reaches the threshold, so they cannot push the account to 100%. They stay held until the close, even if the block resets in the meantime.
- **The patron** is warned on every call while usage is at or above the threshold, before the open and inside the window. The warning reaches Claude as context on the tool call: the 5-hour limit is 96% used, the block resets at 9:10, slow down. The patron is never held for it, so it can still finish work that matters. If it keeps spending, that is its own choice.

Outside the guard the five-hour limit is left alone. With a threshold of 95%, the patron has at least the last 5% of the block to itself. To leave it more, lower `five_hour_threshold` in the policy.

When no block is running, nothing has been used, so nothing is held.

### Where the figures come from

Anthropic offers no API for the remaining quota, but every Claude Code status line sees it. [`continue-claude`](https://github.com/caridina-ai/continue-claude) publishes those figures to a shared file with `-export`:

```json
{
  "statusLine": {
    "type": "command",
    "command": "continue-claude -export X:/claude_usage.json"
  }
}
```

`claude-bouncer` reads `rate_limits.seven_day` and `rate_limits.five_hour` from that file. If the weekly `resets_at` has already passed, the week has reset and its usage is taken as 0%. If the five-hour `resets_at` has passed, no block is running.

Put the usage file and the policy file on a drive that every machine mounts at the same path, such as a NAS share mapped to `X:`. Then one policy covers every machine.

## Install

```powershell
go install github.com/caridina-ai/claude-bouncer/cmd/claude-bouncer@latest
```

This puts `claude-bouncer` in Go's bin directory (`$(go env GOPATH)\bin`), which should be on `PATH`. You also need a status line that publishes the usage file, such as `continue-claude -export` above.

## Policy file

The policy is a JSON file with four required fields and one optional field:

```json
{
  "patron": "trader",
  "start": "8:30",
  "end": "13:30",
  "daily_reserve": 10,
  "five_hour_threshold": 90
}
```

| Field | Meaning |
| --- | --- |
| `patron` | The patron's session name, compared case-insensitively. A session ID works too. |
| `start` | When the exclusive window opens each weekday, `H:MM` in local time. |
| `end` | When it closes. A time earlier than `start` means the window runs past midnight. |
| `daily_reserve` | Percent of the weekly limit kept for the patron for each window left before the weekly reset. |
| `five_hour_threshold` | Optional, default `95`. The five-hour usage, in percent, at which [the five-hour guard](#the-five-hour-guard) holds other sessions and warns the patron. |

A policy written for an older version, without `five_hour_threshold`, works unchanged and uses 95%. Versions before 0.3.0 do not know the field and would treat a policy that has it as invalid, so update every machine before adding it.

A policy with a missing required field or an unknown field is invalid, and an invalid policy switches the bouncer off (see [Failure behavior](#failure-behavior)). After editing it, run `-dry-run` to make sure it is read.

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

## Switch it off

To pause the bouncer, rename the policy file, for example to `claude-bouncer.json.off`. Without a readable policy every call runs, and calls already held are released within a minute. Rename it back to turn the bouncer on again.

To release a single held call, press `Esc` in that session.

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
5-hour    used 30.0%, block 13:20-18:20, threshold 95.0% (default)
guard     off until 11/4 3:30, 5h before the open
decision  hold until 11/4 13:30: weekly reserve for "trader": 15.0% left, 10.0% x 2 window(s) = 20.0% kept until reset 11/5 15:00
```

Add `-now "YYYY-MM-DD HH:MM"` to see what would happen at another time. With other figures in the usage file, the next morning might look like this:

```text
now       2026-11-04 07:40
window    outside 8:30-13:30 Mon-Fri, next opens Wed 8:30
session   research, patron "trader": no
weekly    used 70.0%, left 30.0%, resets 11/5 15:00
reserve   10.0% x 2 window(s) before reset = 20.0%
5-hour    used 96.0%, block 4:10-9:10, threshold 95.0% (default)
guard     on since 3:30, until the close 13:30
decision  hold until 13:30: 5-hour limit 96.0% used (threshold 95.0%) before the window for "trader" opens at 8:30
```

For the patron, at the same moment, the last line reads:

```text
decision  run, and warn the patron: the 5-hour limit is 96.0% used and its block resets at 9:10. Slow down your inference pace, or the 5-hour rate limit may stop you until then.
```

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

When anything goes wrong, the call runs, exactly as it would without the hook, and nothing is printed. A problem never gets in the way of using Claude Code; at worst the bouncer is off.

- A missing, renamed or broken policy file, or an unreachable share, lets every call run. Held calls are released at the next poll.
- An unreadable usage file switches off the weekly reserve and the five-hour guard. The exclusive window still applies, since it needs only the policy. A usage file that lacks one of the two limits switches off only the check for that limit.
- The hook never rejects a call. It only delays it.
- If the Claude Code process exits while a call is held, the hook exits too.

Mistakes on the command line, such as an unknown option, are still reported, so a typo in `settings.json` is noticed instead of silently switching the bouncer off. `-dry-run` shows why the policy could not be read.

## AI provenance

Written by Claude Code using Claude Opus 5.5.

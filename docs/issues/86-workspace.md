# A workspace per run: what #86 delivers

**Issue:** [#86](https://github.com/kiff/agents/issues/86) · **Status:** in progress (the port, the tool and the memory adapter; the AWS adapter and its infra are next).

## What it's for

kiff/tafs#50's setup agent connects whatever tool the owner asks for. It
needs somewhere to work: to write a relay's configuration, read it back and
run commands on it. That place must not become a way around KIFF. So the
workspace has no credentials and no way out, and what reaches outside stays a
gateway tool with a Card.

## Delivered

**The port, `workspace.Workspaces`.** It holds one workspace per run
(`Key{Tenant, Run}`): put, get, list, exec and delete. Every implementation
keeps the same limits and never lets one key reach another's files.

| Limit | Value |
|---|---|
| A path | relative, inside the workspace, printable, 256 bytes |
| A file | 1 MiB |
| A run's files | 16 MiB, 200 files |
| A command's output | 32 KiB each for stdout and stderr; the rest is cut, and the result says so |
| A command's time | 60 s by default, at most 5 minutes |

**The tool, `workspace`.** It has four actions: `write`, `read`, `list` and
`run`. It is an `Own` tool like `memory`, so `tool.Check` is unchanged.
A missing file, a bad path or a limit comes back as a result the model can
correct. A failed or stopped command is a result with `IsError` set.

**Opt-in per definition.** `"workspace": true` gives each run its own
workspace. A definition without it gets no such tool, so tafs' job agents
don't change. `Builder.BuildRun` names the run. Without a run, or on a
server with no workspaces, the build refuses (`ErrNoWorkspace`) rather than
run the agent without one.

```json
PUT /agents/setup
{ "model": "claude-opus-5-5", "workspace": true, … }
```

## Tests that guard it

- `workspacetest`, run by the memory adapter:
  - files read back;
  - a write replaces;
  - lists are sorted;
  - **another tenant, or another run of the same tenant, can't read or list
    the files**;
  - paths are cleaned, and an escape is refused;
  - each limit holds, and replacing a file counts only the difference;
  - delete removes only that workspace;
  - a key with no tenant or run is refused.
- `tool/workspace`:
  - it's `Own` and passes `tool.Check`;
  - the model's mistakes come back as results;
  - a command's exit, its timeout and its cut output are reported;
  - with no command runner, a command answers exit 127.
- `registry`: the tool is there only when the definition asks for it, it's
  bound to the run being built, and the build refuses with no run or no
  workspaces.
- Each was broken on purpose and seen failing:
  - a workspace given without asking;
  - runs sharing one workspace;
  - no total limit;
  - a `..` path allowed.

## Next

- **The AWS adapter:**
  - its own function, `agents-workspace`, with a role that has no permissions, in a VPC with no route out;
  - each run's files in S3 under its run id, with a lifecycle rule;
  - checked against the deployed function: the role can do nothing, and it can't open a connection out.
- **The runner deletes a run's workspace** when the run ends, but not while it's paused.
- **`infra/aws.sh` steps**, one at a time.

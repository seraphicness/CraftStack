# AI agents

The **AI Agents** page (`/ai`) runs Codex or Claude Code on the Master and
streams task output. Administrators authenticate each provider once and
grant selected CraftStack users access by workspace and provider.

## Administrator setup

1. Install Node.js with npm on the Master, then use Install CLIs on the AI
   page, or install the official CLIs on PATH yourself.
2. Select a provider and Login. Follow the provider's browser/device login
   instructions shown in the output. If the CLI requests a code, submit it
   in the login input form. Status and Logout are administrator-only.
3. Register an existing absolute project directory on the Master, or select
   a remote instance and relative subdirectory (use `.` for its root).
4. Grant a user access to that workspace/provider. Enable edit and remote
   apply separately. Apply requires edit. Viewers are always read-only.

Credentials are stored separately from the host user's CLI profile under
`data/ai/auth`, configurable with `CRAFTSTACK_AI_HOME`. Persist and protect
this directory. Login logs and input are available only to administrators;
ordinary users can see only their own tasks for currently authorized targets.
Provider usage is charged to the configured account; use an account and plan
that permit your intended delegated usage.

## Command window and workspaces

Choose a workspace and provider, type a natural-language task and Execute.
The page shows output, errors, history and Cancel. Completed local tasks can
be resumed with a follow-up prompt. Remote jobs start from a fresh snapshot.

- **Local project:** changes are made directly in the registered folder.
  Review the project's Git diff and use your normal version-control workflow.
- **Remote instance:** CraftStack downloads the selected folder to a local
  working copy. Review added, changed and removed files after completion.
  With apply permission, stop the instance and select Apply. CraftStack
  detects changed remote files, creates a full recovery snapshot, then uploads
  changes. Failed uploads trigger rollback. The server is not restarted
  automatically. Small text files include before/after previews; inspect
  larger or binary files separately before applying.

Remote workspaces are limited to 10,000 files and 1 GiB. Prefer a plugin or
configuration subfolder instead of a large world. Recovery snapshots need
space for the whole instance. Work copies and job history are retained on
disk; plan storage and remove obsolete copies during maintenance.

There are at most four concurrent jobs and one active job per workspace.
Tasks time out after 30 minutes; streamed output is bounded to 1 MiB.
Revoking a grant cancels matching active tasks. Interrupted jobs are marked
on Master restart rather than silently resumed.

## Execution boundary

Grant this feature only to trusted operators. These are native CLI processes
running with the Master OS account, not isolated multi-tenant containers.
Workspace grants control CraftStack API access; they are not an OS filesystem
security boundary. Keep unrelated secrets off the worker host and use a
dedicated OS account. In particular, do not expose this facility to mutually
untrusted users who require credential or filesystem isolation.

Prompts go through stdin, never shell command interpolation. Codex uses its
read-only/workspace-write sandbox with noninteractive approvals. Claude uses
Read/Glob/Grep and, for editors, Edit/Write; Bash and external MCP servers are
not enabled. Shell-driven build/test execution is therefore available only
where the provider's configured tools and sandbox permit it. Provider logins
still require a human completing the provider's authentication flow.

See the official [Codex CLI reference](https://developers.openai.com/codex/cli/reference/)
and [Claude Code CLI reference](https://code.claude.com/docs/en/cli-reference).
Tests use fake CLI processes for task streaming and cancellation; they do not
authenticate a real provider account or consume model usage.

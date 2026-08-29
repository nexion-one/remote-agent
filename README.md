# nexion-remote-agent

A small JSON-RPC daemon that [Nexion](https://nexion.one) installs on a remote host so
that opening a project over SSH does not mean one round trip per file listing.

Without it, every directory expansion, every `git status`, every file read is a separate
`ssh` invocation. With it, the Mac opens one SSH connection, speaks JSON-RPC over stdio,
and the agent answers locally on the far end.

## Why this repo exists

Nexion copies this binary onto machines you own and runs it there. Asking someone to do
that with a binary they cannot read is not a reasonable thing to ask, so the source is
here, under the MIT License.

Read `daemon.go` and `rpc.go` first. Together they are roughly 460 lines and cover the whole
lifecycle.

## What it does

- File system reads and writes inside a declared workspace root
- Directory snapshots, including a recursive scanner with subscriptions
- `git` queries and a few mutations: status, branches, commit, push, worktrees
- Search, backed by `rg` when present and `grep` when not
- tmux session control on the remote host

34 methods in total. The dispatch table in `rpc.go` is the complete list; there is no
hidden surface.

## How it runs

The Mac uploads the binary over `scp` to `~/.nexion/bin/nexion-remote-agent`, to a
temporary name first and then `mv` into place, so an interrupted upload cannot leave a
half-written executable. It checks `nexion-remote-agent version` first and skips the
upload when the versions already match.

Two modes:

    nexion-remote-agent daemon --project /path/to/project
    nexion-remote-agent proxy  --project /path/to/project

`daemon` binds a unix socket under `~/.nexion/run/`, named by a hash of the project path.
`proxy` is what SSH actually runs: it starts the daemon if needed and bridges stdio to
that socket. One daemon per project, enforced with `flock`. It exits after ten minutes
without a request.

## Building

    make            # all three targets into dist/
    make linux-amd64
    make vet
    make test

Go 1.22 or newer. No dependencies: `go.mod` has no `require` block and never will, so
`go build` works offline and there is nothing to audit but this repo.

The tests are aimed at the paragraph below rather than at coverage: that traversal, a
NUL, an absolute path and a symlink pointing out of the workspace are all refused, that
a path which only looks like an escape is not, and that the arguments reaching `git` and
`tmux` are held to their character sets.

## Security

There is no listener on any network interface. The socket is a unix socket at mode 0600
inside a 0700 directory, so reaching it means already having that account on that host.

Authentication is SSH. The agent has no credentials, no key material and no auth logic
of its own; it inherits whatever the SSH session already proved. It runs as the
connecting user and does nothing to raise its own privileges.

Every path argument is resolved and checked against the workspace root before use, and
rejected if it is absolute, contains a traversal segment or a NUL. Arguments that reach
`git` are validated against a narrow character set rather than escaped.

If you find something wrong here, see SECURITY.md.

## A wart

Method names are inconsistent: file and git verbs use underscores (`git_worktree_list`),
tmux verbs use a dot (`tmux.list_sessions`). The tmux group was added later and the
naming was not reconciled. Renaming them now would break every deployed agent, so they
stay as they are.

## Contributions

Issues are welcome, particularly about behaviour on hosts I cannot test on. Pull requests
are read, but this repository follows the requirements of the Nexion application, so not
every change can be accepted. Saying that up front beats leaving a branch open for months.

## License

MIT. See LICENSE.

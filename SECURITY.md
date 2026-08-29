# Security

## Reporting

Use GitHub's private vulnerability reporting on this repository
(Security > Report a vulnerability). It stays private until there is a fix.

Please do not open a public issue for anything that lets one user reach another user's
files, escape a workspace root, or run a command that was not requested.

This is maintained by one person. Expect an acknowledgement within a few days rather
than within hours.

## What the agent assumes

The agent trusts its socket completely. Anything that can connect can read and write
inside an opened workspace and run the `git` and tmux verbs. That is by design: the
socket is mode 0600 in a 0700 directory, so a peer on that socket is already the same
local user, who could do all of it without the agent anyway.

It follows that the security boundary is SSH, not the agent. There is no separate
authentication, no token and no key material in this repository.

## In scope

- Escaping the workspace root through a path argument
- Command injection through anything that reaches `git`, `rg` or `tmux`
- One project's daemon serving another project's files
- The socket, PID file or log being created with wider permissions than intended
- A crash reachable from a well-formed request

## Out of scope

- An attacker who already has shell access as that user
- Denial of service by opening many connections
- Anything requiring a modified agent binary on the host

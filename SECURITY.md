# Security Policy

## Reporting a Vulnerability

Please **do not** open a public issue for security vulnerabilities.

Report them through GitHub's private vulnerability reporting:
**Security tab → Advisories → "Report a vulnerability"** in this repository.

You can expect:

- Acknowledgement within 3 business days.
- An initial assessment within 7 days, including whether we accept the report
  and a remediation timeline.
- Credit in the release notes once the fix ships (unless you prefer anonymity).

## Scope

Loom is a library and a CLI that executes user-defined agent graphs and talks
to LLM providers and MCP tool servers. In scope: anything where Loom itself
fails to enforce its own contracts — state isolation between runs, store
transaction guarantees, CLI input handling (prompt parsing, MCP dispatch),
or credential handling in `provider/` and `pgstore/`.

Out of scope: vulnerabilities in LLM providers, MCP servers, or host
applications built on Loom; prompt-injection outcomes inherent to agent
workloads (governing those is the host's job — see the Weave project).

## Supported Versions

Only the latest release receives security fixes. Upgrade to the newest tag.

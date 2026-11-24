# Security

Yggdrasil runs on your own machines and holds sign-ins to your mail, calendar
and chat, and its agents can run code. Please report a vulnerability
privately, not in a public issue.

## Reporting

Use **Report a vulnerability** on the repository's Security tab (GitHub
private vulnerability reporting). Say what you found, how to reproduce it, and
what an attacker could reach. You will get an answer within a week.

## In scope

- A way past a part's bearer token, or a part reachable beyond loopback by default.
- Text from mail, calendar, Slack, Linear or the web that can start, steer or
  approve a coding agent.
- Sign-ins or tokens written where others can read them, or into logs.

Known open hardening work: coding agents are not yet sandboxed, and one owner
token is shared by every part and device.

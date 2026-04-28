# Security Policy

## Reporting a Vulnerability

If you discover a security vulnerability in Pad, **please do not open a public issue.**

Instead, report it privately:

- **Email:** security@perpetualsoftware.org
- **GitHub:** Use [GitHub's private vulnerability reporting](https://github.com/PerpetualSoftware/pad/security/advisories/new)

Please include:

- A description of the vulnerability
- Steps to reproduce it
- The potential impact
- Any suggested fixes (if you have them)

## Response Timeline

- **Acknowledgment:** Within 48 hours
- **Initial assessment:** Within 1 week
- **Fix or mitigation:** Depends on severity, but we aim for:
  - Critical: 72 hours
  - High: 1 week
  - Medium/Low: Next release

## Scope

Pad runs as a local server on the user's machine. Security concerns include:

- **Data integrity** — Pad stores project data in SQLite; unauthorized modification or deletion is a security issue
- **Network exposure** — outside Docker, Pad binds to localhost by default; in Docker, exposure depends on host-side port publishing, and any vulnerability that widens access beyond the intended deployment is in scope
- **Code injection** — Any path where user input (item content, wiki-links, field values) could lead to code execution
- **Path traversal** — Any way to read or write files outside the workspace directory
- **Embedded web UI** — XSS or other web vulnerabilities in the SvelteKit frontend

## Out of Scope

- Vulnerabilities in dependencies (please report those upstream, but let us know so we can update)
- Issues that require physical access to the machine
- Social engineering

## Supported Versions

We provide security fixes for the latest release only.

| Version | Supported |
|---------|-----------|
| Latest  | ✅        |
| Older   | ❌        |

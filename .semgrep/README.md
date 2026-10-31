# `.semgrep/` — deterministic SAST rules (ADR-062 / strategic §7)

This directory holds the project's [Semgrep](https://semgrep.dev) rules. It is
seeded into every project repo from the template.

ADR-062 splits code-quality enforcement into two tiers:

- **Deterministic security/quality rules** — SQL injection, hardcoded
  secrets, insecure dependencies, unsafe patterns — belong here as Semgrep
  rules. They run in CI (and, in future, as a gate step before the LLM
  constraint-agent) and produce structured `CONSTRAINT_VIOLATION` findings.
- **Architectural-pattern enforcement** — layer boundaries, naming, design
  rules that require reasoning about intent — stays with the LLM
  constraint-agent, configured in `agents.yaml` (`agents.constraint-agent`).

Add rule files as `.semgrep/<name>.yml`. A starter rule:

```yaml
rules:
  - id: no-hardcoded-secret
    languages: [typescript, javascript]
    severity: ERROR
    message: Possible hardcoded secret — load it from configuration instead.
    pattern-regex: (?i)(api[_-]?key|secret|password|token)\s*[:=]\s*["'][^"']{8,}["']
```

Until the Semgrep gate step lands, this directory is informational — CI may
already run `semgrep --config .semgrep/`.

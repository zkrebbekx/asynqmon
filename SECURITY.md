# Security policy

## Supported versions

Security fixes go to the latest released minor version. Upgrade to the newest
release before you report a problem, and check that the problem is still
present.

## Reporting a vulnerability

Report a vulnerability through GitHub private vulnerability reporting:

1. Open <https://github.com/zkrebbekx/asynqmon/security/advisories/new>.
2. Describe the problem, the affected version, and the impact.
3. Add the exact steps that reproduce it.

The report stays private until a fix is released. Do not open a public issue
for a vulnerability.

Please include:

- The asynqmon version (`asynqmon --version`) or the image digest.
- The deployment mode: binary, container, or the Helm chart.
- The configuration that matters, with every secret removed: `--read-only`,
  `--auth-header`, `--trusted-proxies`, `--require-identity`,
  `--enable-enqueue`, `--cors-allowed-origins`.
- A request or a payload that reproduces the problem.

Expect a first answer within seven days. This is a volunteer project; the
answer can be slower over a holiday period.

## Scope

In scope:

- Authentication and authorization defects in the API, including the identity
  headers and the read-only mode.
- Injection into Redis commands, AQL queries, or the audit log.
- Cross-site scripting and cross-site request forgery in the Web UI.
- Path traversal or information disclosure from the embedded static assets.
- Secrets that leak into logs, into the API, or into the Helm-rendered
  manifests.

Out of scope:

- A defect in Redis itself, or in a deployment that exposes Redis to the
  internet.
- Running asynqmon without an authenticating reverse proxy. asynqmon performs
  no authentication of its own; see the README.
- Denial of service from a request that a normal operator can already make,
  for example a very large task scan.

## Deployment expectations

asynqmon has no login. Put it behind a reverse proxy that authenticates the
user, restrict the proxy CIDRs with `--trusted-proxies`, and set
`--auth-header` so the audit log names a real user.

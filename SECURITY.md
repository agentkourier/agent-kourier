# Security policy

## Report a vulnerability

Send suspected vulnerabilities to [security@agentkourier.dev](mailto:security@agentkourier.dev).
Do not disclose a suspected vulnerability in a public issue or pull request before a fix and
disclosure date have been agreed with the maintainer.

Include the affected version or commit, deployment configuration, impact, and steps to reproduce.
Use fake credentials and redact private messages, tokens, database connection strings, and other
sensitive data. Ask for a secure transfer method before sending material that cannot be redacted.

## Who handles reports

Travis Bickford, GitHub [@tbickford](https://github.com/tbickford), is the current security response
maintainer. He triages reports, coordinates fixes and releases, and agrees disclosure timing with
the reporter. Adding another security responder and confirming mailbox coverage are follow-up work.

The response targets are an acknowledgement within three business days and a triage update within
seven business days. These are targets, not a staffed support SLA. Fix timing depends on impact,
exploitability, and whether a dependency or backend needs a fix.

## Supported versions

Until the first stable release, security fixes are developed on `main`; prereleases are for
evaluation and have no separate backport commitment. After the first stable release, security
fixes target the latest stable release series. Older series have no backport commitment unless
the release notes explicitly say otherwise. Upgrade to the latest patch release in the supported
series. Report suspected vulnerabilities in any version so the maintainer can assess their impact.

## Fixes and disclosure

The maintainer confirms the affected versions and impact privately, prepares a fix and regression
test, and coordinates a release and disclosure date with the reporter. Reports that affect a
dependency or agent backend are coordinated with that project's maintainers. A dependency issue
still needs an assessment of its impact on Agent Kourier.

Disclosures should state affected and fixed versions, impact, mitigations, and any upgrade steps.
Reporter credit is included only with the reporter's consent. Request a CVE through an appropriate
CVE Numbering Authority when applicable; this project does not claim to be a CNA.

## Release vulnerability policy

Release images must pass vulnerability checks for both supported architectures before publication.
High and critical findings block publication, including findings without an available fix.
Scanner failures also block publication. Do not disable scanning or silently ignore a finding to
cut a release. Evaluate disputed findings privately, document the evidence, and review any proposed
exception with the security response maintainer before changing the policy or scanner configuration.

An image signature identifies the release workflow that signed an artifact. It does not certify
that an artifact has no vulnerabilities. Consumers should verify signatures, deploy by digest,
and assess scan results against their own deployment.

## Scope and deployment

Reports about Agent Kourier code, release artifacts, workflows, and the Helm chart are in scope.
The permissions of an agent's tools and the authentication of an agent backend are also part of
the deployment's trust boundary. Grant a Binding only permissions appropriate for its channel.
Use the documented front door for kagent, isolate Secret namespaces, and configure network policy
for the deployment. See the [security model](https://agentkourier.dev/explanation/security/).

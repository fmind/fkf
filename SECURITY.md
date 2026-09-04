# Security policy

`fkf` stores a concentrated local history of a person's work and can execute commands declared by a trusted base. Please report security defects privately.

## Supported versions

Only the latest published release receives security fixes. Every earlier release is unsupported, including earlier majors and minors. Upgrade before reporting, then confirm the defect still reproduces.

## Report a vulnerability

Open a [private security advisory](https://github.com/fmind/fkf/security/advisories/new). Do not create a public issue before a fix is available.

The same private form accepts sensitive Code of Conduct reports, as documented in [CODE_OF_CONDUCT.md](CODE_OF_CONDUCT.md), even when no software vulnerability is involved.

Include the affected version, operating system, impact, minimal reproduction, and any suggested mitigation. Use a synthetic base and redact credentials, personal identifiers, provider output, absolute home paths, and private repository names. A report should never require access to your real base.

The maintainer will acknowledge reports and coordinate validation, remediation, and disclosure on a best-effort basis. Please allow time for a fix before publishing details.

The documented trust, credential, execution, path, and storage boundaries are in [Privacy and security](https://fmind.github.io/fkf/docs/privacy/). A behavior explicitly listed under its honest limits may be a product constraint rather than a vulnerability, but private reports are still welcome when the impact is unclear.

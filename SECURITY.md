# Security Policy

Note: This project is in early open-source development. We take security seriously but do not currently guarantee response times or patch timelines.

## Supported Versions

We release security fixes for the following versions of this project. If you are running an unsupported version, we strongly encourage you to upgrade before reporting an issue, as it may already be resolved.

| Version | Supported          |
| ------- | ------------------ |
| TBD   | TBD |



## Reporting a Vulnerability

**Please do not report security vulnerabilities through public GitHub issues, pull requests, discussions, or any other public forum.** Public disclosure before a fix is available puts the broader community at risk.

### Preferred Method: GitHub Private Vulnerability Reporting

The preferred way to report a vulnerability is using the **Report a vulnerability** button on the [Security tab](../../security/advisories/new) of this repository. This opens a private, encrypted channel directly with the maintainers and allows us to collaborate on a fix before any public disclosure.

If you are unfamiliar with the process, GitHub's documentation walks through each step:
[Privately reporting a security vulnerability](https://docs.github.com/en/code-security/security-advisories/guidance-on-reporting-and-writing-information-about-vulnerabilities/privately-reporting-a-security-vulnerability)

## What to Include in Your Report

To help us triage and respond as quickly as possible, please include as much of the following as you can:

- A description of the vulnerability and its potential impact
- The affected version(s)
- Step-by-step instructions to reproduce the issue
- Proof-of-concept code or a working exploit, if available
- Any relevant logs, screenshots, or supporting material
- Your assessment of severity (Critical / High / Medium / Low)

The more detail you provide, the faster we can validate and address the issue.

## Credit and Acknowledgment

We believe in recognizing the researchers who help keep this project secure. Unless you request otherwise, we will credit you by name (or handle) in the published security advisory. If you prefer to remain anonymous, please let us know in your report.

## Bug Bounty

**This project does not currently operate a bug bounty program and does not offer monetary rewards for vulnerability reports.** We are grateful for the time and effort security researchers invest in responsibly disclosing issues, and we acknowledge contributions publicly as described above.

We are not ruling out a bug bounty program in the future. If that changes, this document will be updated accordingly.

## Scope and Out-of-Scope Issues

### In Scope

- Vulnerabilities in the source code of this repository at supported versions
- Authentication or authorization flaws
- Injection vulnerabilities (SQL, command, etc.)
- Sensitive data exposure
- Privilege escalation

### Out of Scope

The following are generally not considered in-scope security vulnerabilities for this project:

- Vulnerabilities in third-party dependencies — please report these to the upstream maintainer. If the dependency is a Go module, you may also report it through the [Go vulnerability database](https://pkg.go.dev/vuln/).
- Issues only reproducible with a non-supported version of this project
- Issues flagged solely by automated security scanners without a clear proof of exploitability — please validate findings before reporting
- Social engineering or phishing attacks against maintainers or users
- Denial-of-service attacks requiring sustained access or resources beyond what a legitimate user would have

If you are unsure whether an issue is in scope, please report it privately and we will let you know.

## Security Update Notifications

Security advisories are published under the [Security tab](../../security/advisories) of this repository. To receive notifications, watch this repository and select **Security alerts** from the notification options.
# Security Policy

## Supported versions

This project is pre-1.0 and under active development. Security fixes are applied to the latest released minor version and `main`. Pin a released image tag (not `latest`) and upgrade promptly.

## Reporting a vulnerability

**Do not open a public issue for security vulnerabilities.**

Please report privately using GitHub's [private vulnerability reporting](https://github.com/stubbi/karpenter-provider-hetzner/security/advisories/new) ("Report a vulnerability" on the Security tab). Please include:

- a description of the issue and its impact,
- steps to reproduce or a proof of concept,
- affected version(s) and configuration.

We aim to acknowledge reports within 3 business days and to provide a remediation timeline after triage. Please give us a reasonable window to release a fix before any public disclosure.

## Scope

This provider holds a Hetzner Cloud API token and creates/deletes servers. Relevant concerns include token handling, RBAC, and the blast radius of the controller's permissions. Reports about the chart's default security posture (RBAC scope, pod security context, secret handling) are in scope.

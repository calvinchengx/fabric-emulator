# Unreleased (after v0.36.0)

Draft of what landed on `main` after the `v0.36.0` tag. Rename this file to
`v0.37.0.md` (or whichever minor) when tagging. Open pull requests are not
here.

## Security: the XMLA MWC token was one fixed string

`generateastoken` answered every caller with the same token,
`fabric-emulator-mwc-token`, and XMLA calls carrying it ran as **whoever had
exchanged most recently**.

- **Authentication bypass.** Once anyone had connected over XMLA, a caller with
  no Entra token could send that string and act as them — including an Admin.
- **Identity swap.** Two real callers ran as each other: an Admin's next call
  ran as the Viewer who had connected since.

Each exchange now mints a random token bound to the principal who exchanged,
valid for an hour on the emulator clock; unknown and expired tokens are
refused. The reply shape and the per-connection flow are unchanged, so
SemPy, semantic-link-labs and ADOMD.NET need nothing.
[docs/32](../32-xmla-plan.md#correction-2026-09-14-the-mwc-token-became-a-credential-in-production-not-just-in-the-screens)

## Upgrading

No configuration changes. Consumers pin by digest, so bump
`FABRIC_EMULATOR_VERSION` and `FABRIC_EMULATOR_DIGEST` together.

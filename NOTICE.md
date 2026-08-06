# Notice

**pix** is a Docker, Inc. project, released under the MIT license (see
`LICENSE`). Copyright in the pix source is held by Docker, Inc. and the pix
contributors.

**Inbound contributions.** Contributions to this repository are accepted
inbound under the same terms pix is distributed outbound: the MIT license in
`LICENSE`. Opening a pull request means you license your contribution under
those terms and represent that you have the right to do so (see
`CONTRIBUTING.md`).

## Other vendors' marks

pix is **not affiliated with, endorsed by, or sponsored by** HashiCorp,
Anthropic, OpenAI, Google, Mozilla, or any other third-party vendor whose
product, trademark, or trade name is referenced in this repository, its
documentation, or its generated artifacts (image, binaries, or notices).
Those marks are the property of their respective owners and are used here
only in the ordinary, descriptive sense — to say which components pix builds
on. See `THIRD_PARTY_NOTICES.md` for the components themselves and their
license terms, and `licenses/MPL-2.0.txt` for the verbatim text of the one
weak-copyleft license in that set (MPL-2.0, covering
`github.com/hashicorp/go-plugin` and `github.com/hashicorp/yamux`).

## Docker Hardened Images (DHI)

pix builds `FROM` a Docker Hardened Image base and publishes the resulting
image to `docker.io/<namespace>/pix`. The authorization of record for that
redistribution — and for pix's employer-IP posture generally — is recorded in
`docs/legal/AUTHORIZATIONS.md`. That record is what the release process
relies on; it is not re-derived per build and not asserted by any agent.

Two things that authorization does **not** cover, and this project does not
claim:

- It grants no rights in Docker's trademarks or brand beyond the descriptive
  use above, and no rights to any third party's DHI entitlement.
- A contributor building the image themselves (`make build`) still needs
  their own DHI-entitled Docker account, or the documented public fallback
  base in the `Dockerfile`. pix never requests, stores, or asserts anyone
  else's entitlement.

This notice is provided for transparency and does not constitute legal
advice. See `docs/legal/FINDINGS.md` for the items that still require a
human (counsel or an account holder), not an agent, and
`docs/legal/PRIVACY.md` for what data pix handles.

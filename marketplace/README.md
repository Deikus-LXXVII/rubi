# Rubi store catalog

`catalog.json` lists the plugins reviewed by Rubi-Project. The site workflow signs it with the Rubi release
key and publishes it at `https://rubi-panel.com/marketplace/catalog.json` (with `catalog.json.sig`). Every
Rubi verifies that signature before trusting anything in it.

Each entry pins the publisher's key and, for every reviewed version, the SHA-256 of that release's
`SHA256SUMS`. Rubi installs only those exact builds. See [docs/design/plugins.md](../docs/design/plugins.md).

## Adding a plugin or a version

1. Review the plugin's source at the release tag. Check that every action does what its preview shows,
   that `execute` performs exactly the submitted payload, and that it only connects to its declared hosts.
2. Verify the release: `SHA256SUMS.sig` must verify with the publisher key in `rubi-plugin.json`.
3. Add the version with `sums_sha256` set to `sha256sum SHA256SUMS` of that release, and its `min_rubi`.
4. Update `updated_at` and open a pull request. Merging to `main` publishes the signed catalog.

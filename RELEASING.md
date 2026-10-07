# Releasing

## One-time setup

1. **Release signing key (Ed25519).** Generate it offline and keep the private key safe:

   ```bash
   openssl genpkey -algorithm ed25519 -out release-key.pem
   openssl pkey -in release-key.pem -pubout -out release-key.pub.pem
   ```

   - Commit `release-key.pub.pem`, and paste the same public key into `panel/install.sh` (`RELEASE_KEY`)
     and `internal/integrity/integrity.go` (`ReleaseKey`).
   - Store the private key as the repository secret `RELEASE_SIGNING_KEY`
     (`gh secret set RELEASE_SIGNING_KEY < release-key.pem`), then delete the local copy or keep it offline.
   - Rotating the key means releasing a new installer and binary with the new public key; older binaries
     will report their self-check as failed against releases signed with the new key.

2. **GitHub Pages** (panel and installer at `https://rubi-panel.com`): repository *Settings* > *Pages* >
   *Source: GitHub Actions*, custom domain `rubi-panel.com`, *Enforce HTTPS*. At the DNS provider, point
   the apex domain at GitHub Pages (A records `185.199.108.153`, `185.199.109.153`, `185.199.110.153`,
   `185.199.111.153`).

## Cutting a release

```bash
git tag -a v0.1.0 -m "v0.1.0"
git push origin v0.1.0
```

The `release` workflow tests, builds static binaries for linux/amd64, linux/arm64 and darwin/arm64, writes
`SHA256SUMS` (archives and bare binaries), signs it, verifies the signature against
`release-key.pub.pem`, adds a GitHub build-provenance attestation, publishes the release, and re-runs the
`pages` workflow, which regenerates `https://rubi-panel.com/releases/latest.json`. Installed Rubis read that
file and offer the update to their users.

## Plugins and the store

**First-party plugins** (like `rubi-icloud-mail`) live in their own repositories and are signed with the
*plugin* key, not the release key, so a plugin compromise can't sign Rubi itself.

- One-time: `openssl genpkey -algorithm ed25519 -out plugins-key.pem`, store it as the secret
  `PLUGIN_SIGNING_KEY` in each plugin repository, and commit the public half as `publisher-key.pub.pem`.
  Its base64 body is the plugin manifest's `publisher.key`.
- Release: tag `vX.Y.Z` in the plugin repository. Its workflow builds, writes `rubi-plugin.json` from
  `<plugin> --manifest`, signs `SHA256SUMS`, attests and publishes a (non-pre-)release, and prints the
  SHA-256 of `SHA256SUMS`.

**Listing a version in the store.** Review the release (see `marketplace/README.md`), add it to
`marketplace/catalog.json` with that SHA-256, and merge to `main`. The `pages` workflow signs the catalog
with `RELEASE_SIGNING_KEY` and publishes it at `https://rubi-panel.com/marketplace/catalog.json`. Installed
Rubis see the new version within six hours.

The Go SDK ships with Rubi (`sdk/rubiplugin`): a plugin built against SDK vX works with every Rubi that
supports its `api`.

## Updating the pinned cloudflared

Bump `CLOUDFLARED_VERSION` in `panel/install.sh` and the three checksums. Use the SHA-256 GitHub shows
for each release asset (for macOS that's the `.tgz` archive, not the binary inside it), and verify them by
downloading.

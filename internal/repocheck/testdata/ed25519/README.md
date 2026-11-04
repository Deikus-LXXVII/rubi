# Ed25519 test vectors

`vectors.json` holds test vectors for the Ed25519 verifier inside `panel/install-home.ps1` (the Windows installer). Each
entry has `source`, `name`, `pk`, `msg`, `sig` (hex) and `valid` (true or false).

`gen.py` builds the file. The inputs, and how to run it, are in the header of `gen.py`.

## Sources

- `rfc8032`: the five test vectors of RFC 8032, section 7.1. The text of RFC 8032 belongs to the IETF Trust.
- `wycheproof`: all 210 vectors of `testvectors_v1/ed25519_test.json` from C2SP/wycheproof. Apache License 2.0.
- `speccheck`: the 12 cases of `cases.json` from novifinancial/ed25519-speccheck. Apache License 2.0.
- `derived`: cases made here from RFC 8032 test 2: flipped bits, S + L, wrong lengths, the neutral point as the key and as
  R (canonical, and encoded with y = p + 1), a key with y >= p, a key with x = 0 and the sign bit set.

## What `valid` means

`valid` was computed by a separate verifier written from RFC 8032, section 6 (in `gen.py`). It is strict:

- the public key A and the point R must be canonical encodings: y < p, and x = 0 with the sign bit set is refused;
- S must be less than L;
- the check is cofactorless: `[S]B = R + [k]A`, compared as points.

A vector that Wycheproof marks "acceptable" is decided by this rule.

## Differences from Go's `crypto/ed25519`

Go agrees with `valid` on every vector except two, where Go accepts a non-canonical public key and the installer refuses it:

- `derived`: "the neutral point as the key, encoded with y = p + 1";
- `speccheck`: "case 11" (a key with x = 0 and the sign bit set).

`internal/repocheck/installhomeps1_test.go` fails if any other vector differs, so the file cannot drift unnoticed.

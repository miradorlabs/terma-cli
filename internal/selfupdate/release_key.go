package selfupdate

// releaseKey is terma's release signing public key, key id 5c4bb9d9babad561
// (`go run ./scripts/sign -generate`). The private half is the release workflow's
// TERMA_SIGNING_KEY secret and exists nowhere else.
const releaseKey = "IRUtKmmOlkIxs2eN1A73+MVIRBv7q1EKrv6NmGkfPxo="

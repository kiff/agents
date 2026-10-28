#!/bin/sh
# Signs the e2e test in to KIFF on its own account, on a new phone key, makes
# its setup agent, and puts what the test needs in this repository's secrets:
#
#	E2E_TENANT=<id> infra/e2e-signin.sh
#
# E2E_TENANT is the test's own account's id: an account no person uses, with
# no alert target, so the test's holds and grants reach nobody. A sign-in
# approved on any other account writes nothing and changes no secret.
#
# Run it in your own terminal, from a checkout of kiff/agents, signed in to
# gh with a right to set its secrets. It prints a link: open it signed in to
# that account, and approve within 10 minutes. The session lasts 90 days.
#
# The session's token, the setup agent's key and the phone key go from a 0600
# file straight to `gh secret set` on its standard input, never on a command
# line or the screen, and the file is removed. The secrets:
#
#	E2E_KIFF_SESSION  E2E_SETUP_KEY  E2E_PHONE_KEY
set -eu
cd "$(dirname "$0")/.."
account=${E2E_TENANT:?set it to the test account id}
umask 077
f=$(mktemp)
trap 'rm -f "$f"' EXIT INT TERM
go run ./cmd/e2e -signin "$f" -account "$account"
[ "$(wc -l <"$f")" -eq 3 ] || { echo "the sign-in wrote nothing; no secret changed" >&2; exit 1; }
sed -n 1p "$f" | tr -d '\n' | gh secret set E2E_KIFF_SESSION --repo kiff/agents
sed -n 2p "$f" | tr -d '\n' | gh secret set E2E_SETUP_KEY --repo kiff/agents
sed -n 3p "$f" | tr -d '\n' | gh secret set E2E_PHONE_KEY --repo kiff/agents
rm -f "$f"
echo "E2E_KIFF_SESSION, E2E_SETUP_KEY and E2E_PHONE_KEY are set"

#!/bin/sh
# Signs the e2e test in to KIFF on its own account, on a new phone key, makes
# its setup agent, and puts what the test needs in this repository's secrets.
# Two steps, in this order, because the test's account is a proof test
# account: KIFF takes only keys on its test keys there (any other is refused,
# 409 test_tenant_key), and a session holding a test key lives 14 days.
#
#	infra/e2e-signin.sh key                     1. make the key, print its fingerprints
#	E2E_TENANT=<id> infra/e2e-signin.sh signin  2. once it's on KIFF's test keys: sign in
#
# 1. `key` makes a new phone key in a 0600 file under E2E_KEYS (default
#    ~/.kiff-e2e), and prints its fingerprints. They're added to
#    KIFF_PROOF_TEST_KEYS (the production environment's secret, on the
#    owner's yes), beside the old key's until the new session is set, and
#    KIFF deployed with them. A key file already there is kept, never
#    overwritten: its fingerprint may be on the list already.
# 2. `signin`, after that deploy, signs in with that key. It prints a link:
#    open it signed in to the test's account (E2E_TENANT), and approve within
#    10 minutes. A sign-in approved on any other account writes nothing and
#    changes no secret.
# 3. Then, in this order, never during an Eval or E2E:
#    a. delete the old phone's device from the test's account (an admin, in
#       KIFF), which ends its session;
#    b. only then take the old key's fingerprint off KIFF_PROOF_TEST_KEYS, at
#       a later KIFF change. Taken off while the old device is still on the
#       account, it turns KIFF's use check against every test key there (this
#       phone's, the people's, the proofs') at once.
#
# E2E_TENANT is the test's own account's id: an account no person uses, with
# no alert target, so the test's holds and grants reach nobody.
#
# Run it in your own terminal, from a checkout of kiff/agents, signed in to
# gh with a right to set its secrets. The session's token, the setup agent's
# key and the phone key go from a 0600 file straight to `gh secret set` on
# its standard input, never on a command line or the screen, and the files
# are removed. The secrets:
#
#	E2E_KIFF_SESSION  E2E_SETUP_KEY  E2E_PHONE_KEY
#
# The session lasts 14 days (a test key's): sign in again before then, from
# step 1, so the key changes each time.
set -eu
cd "$(dirname "$0")/.."
step=${1:-}
case $step in key | signin) ;; *) echo "first: key, then signin (see the top of this file)" >&2; exit 2 ;; esac
keys=${E2E_KEYS:-$HOME/.kiff-e2e}
umask 077
mkdir -p "$keys"

if [ "$step" = key ]; then
	if [ -e "$keys/phone.key" ]; then
		echo "a key is already made ($keys/phone.key), kept; remove it to make another"
		exit 0
	fi
	go run ./cmd/e2e -phone-key "$keys/phone.key"
	echo "Next: add these fingerprints to KIFF_PROOF_TEST_KEYS (production environment), on the owner's yes, and deploy KIFF."
	echo "Then: E2E_TENANT=<id> infra/e2e-signin.sh signin"
	exit 0
fi

account=${E2E_TENANT:?set it to the test account id}
[ -f "$keys/phone.key" ] || { echo "no key made yet; run: infra/e2e-signin.sh key" >&2; exit 1; }
f=$(mktemp)
trap 'rm -f "$f"' EXIT INT TERM
echo "(if KIFF answers test_tenant_key in the browser, the key isn't on its test keys yet: add it and deploy first)"
go run ./cmd/e2e -signin "$f" -key "$keys/phone.key" -account "$account"
[ "$(wc -l <"$f")" -eq 3 ] || { echo "the sign-in wrote nothing; no secret changed" >&2; exit 1; }
sed -n 1p "$f" | tr -d '\n' | gh secret set E2E_KIFF_SESSION --repo kiff/agents
sed -n 2p "$f" | tr -d '\n' | gh secret set E2E_SETUP_KEY --repo kiff/agents
sed -n 3p "$f" | tr -d '\n' | gh secret set E2E_PHONE_KEY --repo kiff/agents
rm -f "$f" "$keys/phone.key"
echo "E2E_KIFF_SESSION, E2E_SETUP_KEY and E2E_PHONE_KEY are set."
echo "Next, in this order and never during an Eval: delete the old phone's device from the test's account, then (only then) take its fingerprint off KIFF_PROOF_TEST_KEYS."

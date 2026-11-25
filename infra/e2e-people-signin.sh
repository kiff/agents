#!/bin/sh
# Signs step 9's two people in to KIFF, each on a phone key of their own, and
# puts what the e2e test's -people mode needs in this repository's secrets
# (kiff/tafs#294). Two steps, in this order, because the shared account is a
# test account: KIFF takes only keys on its test keys there (any other is
# refused, 409 test_tenant_key), and one other key would turn every test key
# on it away (E2E, Eval and Proofs at once).
#
#	infra/e2e-people-signin.sh key                    1. make both keys, print their fingerprints
#	E2E_PEOPLE_TENANT=<id> infra/e2e-people-signin.sh signin
#	                                                  2. once both are on KIFF's test keys: sign in
#
# Either step takes one person (maria or luis) after it, to do only theirs.
#
# 1. `key` makes each person a new phone key, in a 0600 file under
#    E2E_PEOPLE_KEYS (default ~/.kiff-e2e-people), and prints its
#    fingerprints. Those are added to KIFF_PROOF_TEST_KEYS (the production
#    environment's secret, on the owner's yes) and KIFF deployed with them.
#    A key file already there is kept, never overwritten: its fingerprint may
#    be on the list already.
# 2. `signin`, after that deploy, signs each person in with their key. For
#    each, it prints a link: that person opens it and approves the sign-in on
#    the shared account (E2E_PEOPLE_TENANT), within 10 minutes. A sign-in
#    approved on any other account writes nothing and changes no secret. It
#    makes no agent. Each session lasts 90 days.
#
# Run it in your own terminal, from a checkout of kiff/agents, signed in to
# gh with a right to set its secrets. The phone key and the session's token
# go from a 0600 file straight to `gh secret set` on its standard input,
# never on a command line or the screen, and the files are removed. The
# secrets:
#
#	E2E_MARIA_PHONE_KEY  E2E_MARIA_SESSION
#	E2E_LUIS_PHONE_KEY   E2E_LUIS_SESSION
set -eu
cd "$(dirname "$0")/.."
step=${1:-}
case $step in key | signin) shift ;; *) echo "first: key, then signin (see the top of this file)" >&2; exit 2 ;; esac
people=${*:-maria luis}
for p in $people; do
	case $p in maria | luis) ;; *) echo "$p: maria or luis" >&2; exit 2 ;; esac
done
keys=${E2E_PEOPLE_KEYS:-$HOME/.kiff-e2e-people}
umask 077
mkdir -p "$keys"

if [ "$step" = key ]; then
	for p in $people; do
		if [ -e "$keys/$p.key" ]; then
			echo "$p: a key is already made ($keys/$p.key), kept; remove it to make another"
			continue
		fi
		go run ./cmd/e2e -people-key "$keys/$p.key" -person "$p"
	done
	echo "Next: add these fingerprints to KIFF_PROOF_TEST_KEYS (production environment), on the owner's yes, and deploy KIFF."
	echo "Then: E2E_PEOPLE_TENANT=<id> infra/e2e-people-signin.sh signin"
	exit 0
fi

account=${E2E_PEOPLE_TENANT:?set it to the shared account id}
for p in $people; do
	[ -f "$keys/$p.key" ] || { echo "$p: no key made yet; run: infra/e2e-people-signin.sh key $p" >&2; exit 1; }
done
for p in $people; do
	P=$(echo "$p" | tr '[:lower:]' '[:upper:]')
	f=$(mktemp)
	trap 'rm -f "$f"' EXIT INT TERM
	echo "== $p"
	echo "(if KIFF answers test_tenant_key in the browser, $p's key isn't on its test keys yet: add it and deploy first)"
	go run ./cmd/e2e -people-signin "$f" -key "$keys/$p.key" -person "$p" -account "$account"
	[ "$(wc -l <"$f")" -eq 2 ] || { echo "$p: the sign-in wrote nothing; no secret changed" >&2; exit 1; }
	sed -n 1p "$f" | tr -d '\n' | gh secret set "E2E_${P}_PHONE_KEY" --repo kiff/agents
	sed -n 2p "$f" | tr -d '\n' | gh secret set "E2E_${P}_SESSION" --repo kiff/agents
	rm -f "$f" "$keys/$p.key"
	echo "$p: E2E_${P}_PHONE_KEY and E2E_${P}_SESSION are set"
done

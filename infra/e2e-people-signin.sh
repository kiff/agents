#!/bin/sh
# Signs step 9's two people in to KIFF, each on a new phone key, and puts
# what the e2e test's -people mode needs in this repository's secrets
# (kiff/tafs#294):
#
#	E2E_PEOPLE_TENANT=<id> infra/e2e-people-signin.sh         both, Maria then Luis
#	E2E_PEOPLE_TENANT=<id> infra/e2e-people-signin.sh luis    one of them again
#
# E2E_PEOPLE_TENANT is the shared account's id: a sign-in approved on any
# other account writes nothing and changes no secret.
#
# Run it in your own terminal, from a checkout of kiff/agents, signed in to
# gh with a right to set its secrets. For each person, it prints a link:
# that person opens it and approves the sign-in on the shared account,
# within 10 minutes. It makes no agent. Each session lasts 90 days.
#
# The phone key and the session's token go from a 0600 file straight to
# `gh secret set` on its standard input, never on a command line or the
# screen, and the file is removed. The secrets:
#
#	E2E_MARIA_PHONE_KEY  E2E_MARIA_SESSION
#	E2E_LUIS_PHONE_KEY   E2E_LUIS_SESSION
set -eu
cd "$(dirname "$0")/.."
account=${E2E_PEOPLE_TENANT:?set it to the shared account id}
people=${*:-maria luis}
for p in $people; do
	case $p in maria | luis) ;; *) echo "$p: maria or luis" >&2; exit 2 ;; esac
done
umask 077
for p in $people; do
	P=$(echo "$p" | tr '[:lower:]' '[:upper:]')
	f=$(mktemp)
	trap 'rm -f "$f"' EXIT INT TERM
	echo "== $p"
	go run ./cmd/e2e -people-signin "$f" -person "$p" -account "$account"
	[ "$(wc -l <"$f")" -eq 2 ] || { echo "$p: the sign-in wrote nothing; no secret changed" >&2; exit 1; }
	sed -n 1p "$f" | tr -d '\n' | gh secret set "E2E_${P}_PHONE_KEY" --repo kiff/agents
	sed -n 2p "$f" | tr -d '\n' | gh secret set "E2E_${P}_SESSION" --repo kiff/agents
	rm -f "$f"
	echo "$p: E2E_${P}_PHONE_KEY and E2E_${P}_SESSION are set"
done

package main

// ownJob reports whether a new taf's result carries a job: the job Setup
// drafted with the owner, its own (kiff/tafs#476: an empty or "custom" job
// with a kickoff and instructions), or, from a Setup before that, one of
// tafs' named jobs ("refunds").
func ownJob(job, kickoff, instructions string) bool {
	if job == "" || job == "custom" {
		return kickoff != "" && instructions != ""
	}
	return job == "refunds"
}

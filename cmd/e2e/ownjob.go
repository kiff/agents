package main

// ownJob reports whether a new taf's result carries a job: the job Setup
// drafted with the owner (kiff/tafs#476), an empty or "custom" job with a
// kickoff, as tool/show requires it; instructions are optional (an
// unscheduled chat taf can carry a kickoff alone).
func ownJob(job, kickoff string) bool {
	return (job == "" || job == "custom") && kickoff != ""
}

// Command evalsuites says which of the Eval's suites a change touches, or
// checks the suites a dispatch asks for (eval/suites.json).
//
//	git -C tafs diff --name-only <base> <head> | go run ./cmd/evalsuites -repo kiff/tafs
//
// prints the suites to dispatch the Eval with, comma-separated: "all" when a
// path touches every suite, nothing when none. With -asked, it checks a
// dispatch's suites and prints them as eval.yml's steps read them
// (",connect,setup," or ",all,"). With -carry, it decides a run of some
// suites for the Upgrade gate from the earlier runs on stdin (a JSON list of
// eval.EarlierRun, newest first, from infra/eval-carry.sh): it prints where
// each other suite's pass came from, or why it has none, and exits 1 when
// one has none.
package main

import (
	"bufio"
	"encoding/json"
	"flag"
	"fmt"
	"os"
	"strings"

	"github.com/kiff/agents/eval"
)

func main() {
	repo := flag.String("repo", "", "the repository the paths are in, as owner/name (kiff/tafs)")
	asked := flag.String("asked", "", "check a dispatch's suites instead: \"connect, setup\", \"all\" or empty")
	checkAsked := flag.Bool("check", false, "with -asked: check and print the suites")
	carry := flag.Bool("carry", false, "with -asked: decide a run of those suites from the earlier runs on stdin")
	flag.Parse()
	s, err := eval.LoadSuites()
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
	if *checkAsked {
		list, err := s.Asked(*asked)
		if err != nil {
			fmt.Fprintln(os.Stderr, err)
			os.Exit(2)
		}
		fmt.Println(list)
		return
	}
	if *carry {
		list, err := s.Asked(*asked)
		if err != nil || list == ","+eval.All+"," {
			fmt.Fprintln(os.Stderr, "-carry is for a run of some suites: -asked names them")
			os.Exit(2)
		}
		var runs []eval.EarlierRun
		if err := json.NewDecoder(os.Stdin).Decode(&runs); err != nil {
			fmt.Fprintln(os.Stderr, "earlier runs:", err)
			os.Exit(2)
		}
		from, short := s.Carry(strings.Split(strings.Trim(list, ","), ","), runs)
		for _, name := range s.Names() {
			if id, ok := from[name]; ok {
				fmt.Printf("%s: passed in run %d, unchanged since\n", name, id)
			} else if why, ok := short[name]; ok {
				fmt.Printf("%s: no pass to carry, %s\n", name, why)
			}
		}
		if len(short) > 0 {
			os.Exit(1)
		}
		return
	}
	if *repo == "" {
		fmt.Fprintln(os.Stderr, "-repo is the repository the paths are in, such as kiff/tafs")
		os.Exit(2)
	}
	var paths []string
	sc := bufio.NewScanner(os.Stdin)
	for sc.Scan() {
		if p := strings.TrimSpace(sc.Text()); p != "" {
			paths = append(paths, p)
		}
	}
	if err := sc.Err(); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
	fmt.Println(strings.Join(s.For(*repo, paths), ","))
}

// Command evalsuites says which of the Eval's suites a change touches, or
// checks the suites a dispatch asks for (eval/suites.json).
//
//	git -C tafs diff --name-only <base> <head> | go run ./cmd/evalsuites -repo kiff/tafs
//
// prints the suites to dispatch the Eval with, comma-separated: "all" when a
// path touches every suite, nothing when none. With -asked, it checks a
// dispatch's suites and prints them as eval.yml's steps read them
// (",connect,setup," or ",all,").
package main

import (
	"bufio"
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

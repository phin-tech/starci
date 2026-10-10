package step

import (
	"encoding/xml"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"git.phin.tech/sam/starci/internal/plan"
)

// maxReport is how large one report may be. A report is read whole.
const maxReport = 16 << 20

// A report is JUnit XML, which is less a format than a habit: a testsuite
// of testcases, sometimes inside testsuites, sometimes nested, with a
// failure or an error or a skipped inside a testcase that did not pass.
// What follows reads what every writer of it agrees on and ignores the rest.

type junitSuite struct {
	Name   string       `xml:"name,attr"`
	File   string       `xml:"file,attr"`
	Suites []junitSuite `xml:"testsuite"`
	Cases  []junitCase  `xml:"testcase"`
}

type junitCase struct {
	Name      string        `xml:"name,attr"`
	Class     string        `xml:"classname,attr"`
	File      string        `xml:"file,attr"`
	Time      float64       `xml:"time,attr"`
	Failure   *junitProblem `xml:"failure"`
	Error     *junitProblem `xml:"error"`
	Skipped   *struct{}     `xml:"skipped"`
	Skipped2  *struct{}     `xml:"skip"`
	Status    string        `xml:"status,attr"`
	IsSkipped string        `xml:"skipped,attr"`
}

type junitProblem struct {
	Message string `xml:"message,attr"`
	Type    string `xml:"type,attr"`
	Text    string `xml:",chardata"`
}

// parseReport reads one report into the tests it holds.
func parseReport(r io.Reader) ([]plan.TestCase, error) {
	// The root is a testsuites holding suites, or one suite on its own.
	// Either way it is a thing with testsuite and testcase children.
	var root junitSuite
	dec := xml.NewDecoder(r)
	// A report is data, not a document: nothing in it is fetched or
	// expanded, and one that asks for either is refused.
	dec.Strict = true
	dec.Entity = nil
	if err := dec.Decode(&root); err != nil {
		return nil, err
	}
	var cases []plan.TestCase
	var walk func(s junitSuite, file string)
	walk = func(s junitSuite, file string) {
		if s.File != "" {
			file = s.File
		}
		for _, c := range s.Cases {
			tc := plan.TestCase{Name: c.Name, Class: c.Class, Seconds: c.Time, Status: "passed", File: c.File}
			if tc.File == "" {
				tc.File = file
			}
			switch problem := firstProblem(c); {
			case problem != nil:
				tc.Status, tc.Message = "failed", oneLine(problem)
			case c.Skipped != nil || c.Skipped2 != nil || c.Status == "skipped" || c.IsSkipped == "true":
				tc.Status = "skipped"
			}
			cases = append(cases, tc)
		}
		for _, inner := range s.Suites {
			walk(inner, file)
		}
	}
	walk(root, "")
	return cases, nil
}

func firstProblem(c junitCase) *junitProblem {
	if c.Failure != nil {
		return c.Failure
	}
	return c.Error
}

// oneLine is why a test failed, cut to a line that fits where it is shown.
func oneLine(p *junitProblem) string {
	msg := strings.TrimSpace(p.Message)
	if msg == "" {
		msg = strings.TrimSpace(p.Text)
	}
	if msg == "" {
		msg = p.Type
	}
	msg, _, _ = strings.Cut(msg, "\n")
	if len(msg) > 300 {
		msg = msg[:300] + "…"
	}
	return msg
}

// readReports reads the reports a step named from its workspace and returns
// what they said, or nil if none could be read.
//
// A report is written by the step, and is read on the worker's side of the
// workspace, so it is read as an output is: through a root that will not
// follow a path out of the workspace, and never through a link.
//
// A report that is missing or cannot be understood is worth a line in the
// log and no more. Reports are there to explain a step, not to fail one.
func readReports(workspace string, reports []string, log func(string)) *plan.Tests {
	if len(reports) == 0 {
		return nil
	}
	root, err := os.OpenRoot(workspace)
	if err != nil {
		return nil
	}
	defer root.Close()

	var all []plan.TestCase
	read := false
	for _, rel := range reports {
		cases, err := readReport(root, rel)
		if err != nil {
			log(fmt.Sprintf("warning: the test report %s was not read: %v", rel, err))
			continue
		}
		read = true
		all = append(all, cases...)
	}
	if !read {
		return nil
	}
	tests := &plan.Tests{}
	for _, c := range all {
		switch c.Status {
		case "failed":
			tests.Failed++
		case "skipped":
			tests.Skipped++
		default:
			tests.Passed++
		}
	}
	// Failed ones first, so that they are what is kept when there are too
	// many to keep them all; otherwise in the order the reports gave.
	sort.SliceStable(all, func(i, j int) bool { return all[i].Status == "failed" && all[j].Status != "failed" })
	if len(all) > plan.MaxTestCases {
		all, tests.Truncated = all[:plan.MaxTestCases], true
	}
	tests.Cases = all
	return tests
}

func readReport(root *os.Root, rel string) ([]plan.TestCase, error) {
	// No link anywhere on the way, as for an output.
	parts := strings.Split(rel, "/")
	for i := 1; i <= len(parts); i++ {
		info, err := root.Lstat(filepath.FromSlash(strings.Join(parts[:i], "/")))
		if err != nil {
			if os.IsNotExist(err) {
				return nil, fmt.Errorf("the step did not write it")
			}
			return nil, err
		}
		if info.Mode()&fs.ModeSymlink != 0 {
			return nil, fmt.Errorf("%s is a link, and a report is not read through one", strings.Join(parts[:i], "/"))
		}
		if i == len(parts) && !info.Mode().IsRegular() {
			return nil, fmt.Errorf("it is not a file")
		}
		if i == len(parts) && info.Size() > maxReport {
			return nil, fmt.Errorf("it is larger than the %d MB a report may be", maxReport>>20)
		}
	}
	f, err := root.Open(filepath.FromSlash(rel))
	if err != nil {
		return nil, err
	}
	defer f.Close()
	cases, err := parseReport(io.LimitReader(f, maxReport))
	if err != nil {
		return nil, fmt.Errorf("it is not JUnit XML: %v", err)
	}
	return cases, nil
}

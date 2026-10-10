package plan

import (
	"fmt"
	"strings"

	"go.starlark.net/starlark"
	"go.starlark.net/starlarkstruct"
)

// Tests is what a step's test reports said, when it named some.
type Tests struct {
	Passed  int `json:"passed"`
	Failed  int `json:"failed"`
	Skipped int `json:"skipped"`
	// Cases is each test, failed ones first. A report can hold more tests
	// than are worth carrying about, so past MaxTestCases the rest are left
	// out and Truncated says so; the counts above are still of all of them.
	Cases     []TestCase `json:"cases,omitempty"`
	Truncated bool       `json:"truncated,omitempty"`
}

// MaxTestCases is how many tests a result carries by name.
const MaxTestCases = 2000

// TestCase is one test.
type TestCase struct {
	// Name is the test's name as its report gives it, and Class the class,
	// module or package the report puts it in. They are kept apart because
	// what a test runner is asked to run again is spelled differently by
	// each: "tests/test_math.py::test_divides" for one, "TestDivides" for
	// another. A script puts the two together the way its runner wants.
	Name  string `json:"name"`
	Class string `json:"class,omitempty"`
	// File is where the test is, as far as the report says: a file, or
	// failing that the class or package it belongs to.
	File    string  `json:"file,omitempty"`
	Seconds float64 `json:"seconds,omitempty"`
	// Status is passed, failed or skipped.
	Status string `json:"status"`
	// Message is why it failed, in a line.
	Message string `json:"message,omitempty"`
}

// Display is the test's name for a person to read: its class and its name,
// which between them tell two tests of the same name apart.
func (c TestCase) Display() string {
	if c.Class == "" || strings.HasPrefix(c.Name, c.Class+".") {
		return c.Name
	}
	return c.Class + "." + c.Name
}

// FailedNames returns the tests that failed, each as Display gives it, in
// the order the reports gave them.
func (t *Tests) FailedNames() []string {
	if t == nil {
		return nil
	}
	var names []string
	for _, c := range t.Cases {
		if c.Status == "failed" {
			names = append(names, c.Display())
		}
	}
	return names
}

// Naming says which tests failed in a few words, for a place with little
// room: the first few by name and the rest as a number. Empty when none did.
func (t *Tests) Naming(few int) string {
	names := t.FailedNames()
	if t == nil || t.Failed == 0 {
		return ""
	}
	shown := names
	if len(shown) > few {
		shown = shown[:few]
	}
	out := strings.Join(shown, ", ")
	if more := t.Failed - len(shown); more > 0 {
		if out == "" {
			return fmt.Sprintf("%d tests", t.Failed)
		}
		out += fmt.Sprintf(" and %d more", more)
	}
	return out
}

// testsValue is result.tests: the tests by how they went, each a list a
// script can loop over. A step with no reports has empty lists, so a script
// need not ask first whether there are any.
func testsValue(t *Tests) starlark.Value {
	lists := map[string][]starlark.Value{"passed": {}, "failed": {}, "skipped": {}}
	counts := Tests{}
	if t != nil {
		counts = *t
		for _, c := range t.Cases {
			lists[c.Status] = append(lists[c.Status], starlarkstruct.FromStringDict(starlark.String("test"), starlark.StringDict{
				"name": starlark.String(c.Name),
				// Not "class", which a script may not write.
				"classname": starlark.String(c.Class),
				"file":      starlark.String(c.File),
				"seconds":   starlark.Float(c.Seconds),
				"message":   starlark.String(c.Message),
			}))
		}
	}
	return starlarkstruct.FromStringDict(starlark.String("tests"), starlark.StringDict{
		"passed":  starlark.NewList(lists["passed"]),
		"failed":  starlark.NewList(lists["failed"]),
		"skipped": starlark.NewList(lists["skipped"]),
		// How many there were, which is more than the lists hold when a
		// report was too long to carry whole.
		"count":     starlark.MakeInt(counts.Passed + counts.Failed + counts.Skipped),
		"truncated": starlark.Bool(counts.Truncated),
	})
}

package plan

import "testing"

func TestAStepSaysWhatItMayUse(t *testing.T) {
	p := mustEval(t, `
def ci(ctx):
    return [
        step("big", image = "i", run = "r", timeout = "1h30m", cpus = 2, memory = "4g"),
        step("small", image = "i", run = "r", timeout = "90s", cpus = 0.5, memory = "512m"),
        step("unsaid", image = "i", run = "r"),
    ]
`)
	big, small, unsaid := p.Steps[0], p.Steps[1], p.Steps[2]
	if big.Timeout != 5400 || big.CPUs != 2 || big.Memory != 4<<30 {
		t.Errorf("big = %d s, %v cpus, %d bytes", big.Timeout, big.CPUs, big.Memory)
	}
	if small.Timeout != 90 || small.CPUs != 0.5 || small.Memory != 512<<20 {
		t.Errorf("small = %d s, %v cpus, %d bytes", small.Timeout, small.CPUs, small.Memory)
	}
	if unsaid.Timeout != 0 || unsaid.CPUs != 0 || unsaid.Memory != 0 {
		t.Errorf("a step that says nothing = %+v, want no limits of its own", unsaid)
	}
	if err := p.Validate(); err != nil {
		t.Errorf("Validate: %v", err)
	}
}

func TestLimitsThatCannotBe(t *testing.T) {
	for arg, fragment := range map[string]string{
		`timeout = "soon"`:  "is not a length of time",
		`timeout = "10"`:    "is not a length of time",
		`timeout = "500ms"`: "whole number of seconds",
		`timeout = "0s"`:    "whole number of seconds",
		`timeout = "-5m"`:   "whole number of seconds",
		`cpus = 0`:          "cpus must be a number above zero",
		`cpus = -1`:         "cpus must be a number above zero",
		`cpus = "2"`:        "cpus must be a number above zero",
		`memory = "lots"`:   "is not an amount of memory",
		`memory = "4"`:      "is not an amount of memory",
		`memory = "0g"`:     "is not an amount of memory",
		`memory = "4gb"`:    "is not an amount of memory",
		`memory = "1m"`:     "under which a container does not start",
	} {
		wantError(t, "def ci(ctx):\n    return [step(\"a\", image = \"i\", run = \"r\", "+arg+")]\n", fragment)
	}
}

func TestLimitsAreWrittenAsTheyAreRead(t *testing.T) {
	for _, s := range []string{"45s", "10m", "1h", "1h30m", "1m30s", "24h"} {
		n, err := ParseTimeout(s)
		if err != nil || FormatTimeout(n) != s {
			t.Errorf("ParseTimeout(%q) = %d, %v; written back as %q", s, n, err, FormatTimeout(n))
		}
	}
	for _, s := range []string{"512m", "4g", "6m", "1536m", "8191k"} {
		n, err := ParseMemory(s)
		if err != nil || FormatMemory(n) != s {
			t.Errorf("ParseMemory(%q) = %d, %v; written back as %q", s, n, err, FormatMemory(n))
		}
	}
}

package local

import (
	"context"
	"strings"
	"testing"

	"git.phin.tech/sam/starci/internal/engine"
	"git.phin.tech/sam/starci/internal/engine/enginetest"
	"git.phin.tech/sam/starci/internal/plan"
	"git.phin.tech/sam/starci/internal/step"
)

func TestEngine(t *testing.T) {
	enginetest.Run(t, &Engine{Config: step.Config{Worker: "test", StateDir: t.TempDir()}, Slots: 4})
}

const gateScript = `
def pipeline(ctx):
    answer = wait_for("approve deploy")
    if not answer.approved:
        fail("refused by " + answer.by)
`

func gateRun() engine.Run {
	return engine.Run{Mode: plan.Advanced, Filename: "ci.star", Source: []byte(gateScript), Context: plan.Context{Repo: "sam/app", SHA: "abc"}}
}

// On one machine a script can wait only for someone who is there.
func TestALocalRunAsksWhoeverIsThere(t *testing.T) {
	var asked plan.Gate
	eng := &Engine{Ask: func(g plan.Gate) (plan.Answer, error) {
		asked = g
		return plan.Answer{Approved: true, By: "sam"}, nil
	}}
	if missing := strings.Join(eng.Can().Missing(gateRun()), " "); !eng.Can().Waits || strings.Contains(missing, "waits for a person") {
		t.Errorf("an engine with someone to ask says it cannot wait: %s", missing)
	}
	result, err := eng.Execute(context.Background(), gateRun())
	if err != nil || !result.OK {
		t.Fatalf("approved: %+v, %v", result, err)
	}
	if asked.Name != "approve deploy" || asked.Repo != "sam/app" || asked.SHA != "abc" {
		t.Errorf("asked about %+v", asked)
	}

	eng.Ask = func(plan.Gate) (plan.Answer, error) { return plan.Answer{By: "alex"}, nil }
	if result, _ := eng.Execute(context.Background(), gateRun()); result.OK || !strings.Contains(result.Error, "refused by alex") {
		t.Errorf("refused: %+v", result)
	}
}

func TestALocalRunWithNobodyToAskSaysSoFirst(t *testing.T) {
	eng := &Engine{}
	missing := eng.Can().Missing(gateRun())
	if eng.Can().Waits || len(missing) == 0 || !strings.Contains(strings.Join(missing, " "), "waits for a person") {
		t.Errorf("before the run, missing = %v; want it to say the script waits and nobody is there", missing)
	}
	result, err := eng.Execute(context.Background(), gateRun())
	if err != nil || result.OK || !strings.Contains(result.Error, "there is nobody to ask") {
		t.Errorf("%+v, %v; want the run failed at the gate, saying why", result, err)
	}
	// A script that does not wait is not warned about.
	plain := gateRun()
	plain.Source = []byte("def pipeline(ctx):\n    pass\n")
	for _, m := range eng.Can().Missing(plain) {
		if strings.Contains(m, "waits for a person") {
			t.Errorf("a script with no gate is said to wait: %s", m)
		}
	}
}

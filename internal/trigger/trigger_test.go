package trigger

import (
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"git.phin.tech/sam/starci/internal/forge"
	"git.phin.tech/sam/starci/internal/plan"
)

const secret = "shared-secret"

// fakeForgejo is a Forgejo API that serves one file and records statuses.
type fakeForgejo struct {
	*httptest.Server
	script   string // "" means the repository has no script
	statuses []map[string]string
	down     bool
}

func newFakeForgejo(t *testing.T, script string) *fakeForgejo {
	t.Helper()
	f := &fakeForgejo{script: script}
	f.Server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if f.down {
			http.Error(w, "down", http.StatusServiceUnavailable)
			return
		}
		if r.Header.Get("Authorization") != "token tok" {
			http.Error(w, "no token", http.StatusUnauthorized)
			return
		}
		switch {
		case r.Method == "GET" && r.URL.Path == "/api/v1/repos/sam/app/raw/.ci.star":
			if f.script == "" || r.URL.Query().Get("ref") != "abc123" {
				http.NotFound(w, r)
				return
			}
			w.Write([]byte(f.script))
		case r.Method == "POST" && r.URL.Path == "/api/v1/repos/sam/app/statuses/abc123":
			var s map[string]string
			json.NewDecoder(r.Body).Decode(&s)
			f.statuses = append(f.statuses, s)
			w.WriteHeader(http.StatusCreated)
		default:
			http.NotFound(w, r)
		}
	}))
	t.Cleanup(f.Close)
	return f
}

func (f *fakeForgejo) last() string {
	if len(f.statuses) == 0 {
		return "(none)"
	}
	s := f.statuses[len(f.statuses)-1]
	return s["state"] + ": " + s["description"]
}

type submitted struct {
	runs []Run
	err  error
}

func (s *submitted) Submit(_ context.Context, r Run) error {
	if s.err != nil {
		return s.err
	}
	s.runs = append(s.runs, r)
	return nil
}

func pushBody(ref, after string) []byte {
	b, _ := json.Marshal(map[string]any{
		"ref": ref, "after": after,
		"repository": map[string]string{"full_name": "sam/app", "clone_url": "https://git.example/sam/app.git"},
		"commits": []map[string][]string{
			{"added": {"src/new.py"}, "modified": {"README.md"}},
			{"modified": {"src/new.py", "ui/app.ts"}, "removed": {"old.txt"}},
		},
	})
	return b
}

func sign(body []byte, key string) string {
	mac := hmac.New(sha256.New, []byte(key))
	mac.Write(body)
	return hex.EncodeToString(mac.Sum(nil))
}

// wired is a door connected straight to a receiver, which is what a forge
// sees however the two are really joined up.
type wired struct {
	door *Door
	recv *Receiver
	// said is what the receiver made of the last delivery it was handed.
	said string
}

func deliver(h *wired, event string, body []byte, signature string) *httptest.ResponseRecorder {
	h.said = ""
	req := httptest.NewRequest("POST", "/hooks/forgejo", strings.NewReader(string(body)))
	req.Header.Set("X-Forgejo-Event", event)
	req.Header.Set("X-Forgejo-Signature", signature)
	rec := httptest.NewRecorder()
	h.door.ServeHTTP(rec, req)
	return rec
}

func setup(t *testing.T, script string) (*wired, *fakeForgejo, *submitted) {
	t.Helper()
	fake := newFakeForgejo(t, script)
	sub := &submitted{}
	f := forge.Forgejo{BaseURL: fake.URL, Token: "tok"}
	h := &wired{recv: &Receiver{
		Hooks:  map[string]Hook{"forgejo": {Forge: f, Secret: []byte(secret)}},
		Submit: sub,
	}}
	// The door is given no secret, so every check of a signature in these
	// tests is the receiver's.
	h.door = &Door{
		Forges: map[string]forge.Forge{"forgejo": f},
		Send: func(ctx context.Context, d Delivery) error {
			said, err := h.recv.Deliver(ctx, d)
			h.said = said
			return err
		},
	}
	return h, fake, sub
}

const basicScript = `
def ci(ctx):
    return [step("test-" + d, image = "i", run = "r") for d in ctx.changed_dirs if d != "."]
`

func TestPushStartsARun(t *testing.T) {
	h, fake, sub := setup(t, basicScript)
	body := pushBody("refs/heads/main", "abc123")
	rec := deliver(h, "push", body, sign(body, secret))

	if rec.Code != http.StatusAccepted {
		t.Fatalf("status %d: %s", rec.Code, rec.Body)
	}
	if len(sub.runs) != 1 {
		t.Fatalf("%d runs submitted, want 1", len(sub.runs))
	}
	run := sub.runs[0]
	// The plan follows the files the push changed, taken from the delivery.
	var names []string
	for _, s := range run.Plan.Steps {
		names = append(names, s.Name)
	}
	if got, want := strings.Join(names, " "), "test-src test-ui"; got != want {
		t.Errorf("steps = %s, want %s", got, want)
	}
	if run.Supersedes != "sam/app@main" || run.CloneURL != "https://git.example/sam/app.git" || run.Context.SHA != "abc123" || run.Context.Event != plan.EventPush {
		t.Errorf("run = %+v", run)
	}
	if run.Report != (Report{Forge: "forgejo", Repo: "sam/app", SHA: "abc123", Context: "starci"}) {
		t.Errorf("report = %+v", run.Report)
	}
	if got, want := fake.last(), "pending: queued: 2 step(s)"; got != want {
		t.Errorf("status = %q, want %q", got, want)
	}
	if fake.statuses[0]["context"] != "starci" {
		t.Errorf("status context = %q", fake.statuses[0]["context"])
	}
}

func TestAdvancedScriptIsSubmittedAsSource(t *testing.T) {
	src := "def pipeline(ctx):\n    pass\n"
	h, fake, sub := setup(t, src)
	body := pushBody("refs/heads/main", "abc123")
	deliver(h, "push", body, sign(body, secret))
	if len(sub.runs) != 1 || sub.runs[0].Mode != plan.Advanced || string(sub.runs[0].Source) != src {
		t.Fatalf("runs = %+v", sub.runs)
	}
	if got := fake.last(); got != "pending: queued" {
		t.Errorf("status = %q", got)
	}
}

func TestUnsignedDeliveriesAreRefused(t *testing.T) {
	h, fake, sub := setup(t, basicScript)
	body := pushBody("refs/heads/main", "abc123")
	for name, sig := range map[string]string{
		"no signature":       "",
		"wrong secret":       sign(body, "someone-else"),
		"signature of other": sign([]byte("different body"), secret),
		"not hex":            "zzzz",
	} {
		if rec := deliver(h, "push", body, sig); rec.Code != http.StatusUnauthorized {
			t.Errorf("%s: status %d, want 401", name, rec.Code)
		}
	}
	if len(sub.runs) != 0 || len(fake.statuses) != 0 {
		t.Errorf("an unsigned delivery had an effect: %d runs, %d statuses", len(sub.runs), len(fake.statuses))
	}
}

func TestDeliveriesWithNothingToRunAreIgnored(t *testing.T) {
	h, _, sub := setup(t, basicScript)
	cases := map[string]struct {
		event string
		body  []byte
	}{
		"another event":         {"issues", pushBody("refs/heads/main", "abc123")},
		"a deleted branch":      {"push", pushBody("refs/heads/gone", strings.Repeat("0", 40))},
		"a deleted tag":         {"push", pushBody("refs/tags/v1", strings.Repeat("0", 40))},
		"a closed pull request": {"pull_request", prBody("closed", 7, "sam/app")},
		"a labelled one":        {"pull_request", prBody("labeled", 7, "sam/app")},
	}
	for name, c := range cases {
		rec := deliver(h, c.event, c.body, sign(c.body, secret))
		if rec.Code != http.StatusAccepted || !strings.HasPrefix(h.said, "ignored") {
			t.Errorf("%s: status %d, receiver said %q; want it accepted and ignored", name, rec.Code, h.said)
		}
	}
	if len(sub.runs) != 0 {
		t.Errorf("%d runs submitted for ignored deliveries", len(sub.runs))
	}
}

func TestRepositoryWithoutAScript(t *testing.T) {
	h, fake, sub := setup(t, "")
	body := pushBody("refs/heads/main", "abc123")
	rec := deliver(h, "push", body, sign(body, secret))
	if rec.Code != http.StatusAccepted || len(sub.runs) != 0 || len(fake.statuses) != 0 {
		t.Errorf("status %d, %d runs, %d statuses; want accepted and nothing else", rec.Code, len(sub.runs), len(fake.statuses))
	}
}

func TestBrokenScriptIsReportedOnTheCommit(t *testing.T) {
	h, fake, sub := setup(t, "def ci(ctx):\n    return [step(\"a\", image = \"i\", run = \"r\", needs = [\"zz\"])]\n")
	body := pushBody("refs/heads/main", "abc123")
	rec := deliver(h, "push", body, sign(body, secret))
	if rec.Code != http.StatusAccepted || len(sub.runs) != 0 {
		t.Fatalf("status %d, %d runs", rec.Code, len(sub.runs))
	}
	if got := fake.last(); !strings.HasPrefix(got, "failure: ") || !strings.Contains(got, `needs "zz"`) {
		t.Errorf("status = %q, want a failure naming the mistake", got)
	}
}

func TestEmptyPlanPassesWithoutARun(t *testing.T) {
	h, fake, sub := setup(t, "def ci(ctx):\n    return []\n")
	body := pushBody("refs/heads/main", "abc123")
	deliver(h, "push", body, sign(body, secret))
	if len(sub.runs) != 0 || fake.last() != "success: nothing to run for this commit" {
		t.Errorf("%d runs, status %q", len(sub.runs), fake.last())
	}
}

func TestFailureToQueueIsAnErrorTheForgeCanRetry(t *testing.T) {
	h, fake, sub := setup(t, basicScript)
	sub.err = errors.New("engine unreachable")
	body := pushBody("refs/heads/main", "abc123")
	rec := deliver(h, "push", body, sign(body, secret))
	if rec.Code != http.StatusBadGateway {
		t.Errorf("status %d, want 502 so the forge retries", rec.Code)
	}
	if got := fake.last(); got != "error: could not be queued: engine unreachable" {
		t.Errorf("status = %q", got)
	}

	h2, fake2, _ := setup(t, basicScript)
	fake2.down = true
	if rec := deliver(h2, "push", body, sign(body, secret)); rec.Code != http.StatusBadGateway {
		t.Errorf("with the forge API down: status %d, want 502", rec.Code)
	}
}

func TestGitHubDeliveries(t *testing.T) {
	g := forge.GitHub{Token: "tok"}
	body := pushBody("refs/heads/main", "abc123")
	h := http.Header{}
	h.Set("X-GitHub-Event", "push")
	h.Set("X-Hub-Signature-256", "sha256="+sign(body, secret))
	push, err := forge.ParsePush(g, h, body, []byte(secret))
	if err != nil || push.Repo != "sam/app" || push.Ref != "main" || push.SHA != "abc123" {
		t.Fatalf("push = %+v, %v", push, err)
	}
	if got, want := strings.Join(push.Changed, " "), "README.md old.txt src/new.py ui/app.ts"; got != want {
		t.Errorf("changed = %s, want %s", got, want)
	}
	h.Set("X-Hub-Signature-256", sign(body, secret)) // without the sha256= prefix
	if _, err := forge.ParsePush(g, h, body, []byte(secret)); !errors.Is(err, forge.ErrSignature) {
		t.Errorf("a signature in the wrong form was accepted: %v", err)
	}
	if _, err := forge.ParsePush(g, h, body, nil); !errors.Is(err, forge.ErrSignature) {
		t.Errorf("a delivery was accepted with no secret configured: %v", err)
	}
}

// A door that knows the secret turns an unsigned delivery away itself, and
// one that does not passes it on to be turned away. Either way it goes no
// further.
func TestTheDoorNeedNotBeTrusted(t *testing.T) {
	body := pushBody("refs/heads/main", "abc123")

	h, _, sub := setup(t, basicScript)
	passed := 0
	inner := h.door.Send
	h.door.Send = func(ctx context.Context, d Delivery) error { passed++; return inner(ctx, d) }

	if rec := deliver(h, "push", body, sign(body, "wrong")); rec.Code != http.StatusUnauthorized || passed != 1 {
		t.Errorf("without the secret: status %d, passed on %d times; want 401 from the receiver", rec.Code, passed)
	}
	h.door.Secrets = map[string][]byte{"forgejo": []byte(secret)}
	if rec := deliver(h, "push", body, sign(body, "wrong")); rec.Code != http.StatusUnauthorized || passed != 1 {
		t.Errorf("with the secret: status %d, passed on %d times; want 401 at the door", rec.Code, passed)
	}
	if len(sub.runs) != 0 {
		t.Errorf("%d runs started by unsigned deliveries", len(sub.runs))
	}

	// What a door passes on is the delivery exactly as it arrived.
	var got Delivery
	h.door.Send = func(_ context.Context, d Delivery) error { got = d; return nil }
	deliver(h, "push", body, sign(body, secret))
	if got.Forge != "forgejo" || got.Event != "push" || got.Signature != sign(body, secret) || got.Body != string(body) {
		t.Errorf("passed on %+v", got)
	}
}

// An engine's own webhook endpoint checks the signature and hands over only
// the body. What happened has to be read from that.
func TestDeliveriesAlreadyVerified(t *testing.T) {
	h, fake, sub := setup(t, eventScript)
	for _, c := range []struct {
		body []byte
		want string
	}{
		{pushBody("refs/heads/main", "abc123"), "push"},
		{pushBody("refs/tags/v2", "abc123"), "release v2"},
		{prBody("opened", 7, "sam/app"), "pr 7 fix-login -> main"},
	} {
		sub.runs = nil
		if _, err := h.recv.DeliverVerified(context.Background(), "forgejo", c.body); err != nil {
			t.Fatal(err)
		}
		if len(sub.runs) != 1 || sub.runs[0].Plan.Steps[0].Name != c.want {
			t.Errorf("runs = %+v, want one with the step %q", sub.runs, c.want)
		}
	}
	if said, _ := h.recv.DeliverVerified(context.Background(), "forgejo", []byte(`{"zen":"hello"}`)); !strings.HasPrefix(said, "ignored") {
		t.Errorf("a delivery that is neither a push nor a pull request led to %q", said)
	}
	_ = fake
}

func TestGitHubAPI(t *testing.T) {
	var gotStatus map[string]string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Bearer tok" {
			http.Error(w, "no token", http.StatusUnauthorized)
			return
		}
		switch {
		case r.Method == "GET" && r.URL.Path == "/repos/sam/app/contents/.ci.star" && r.URL.Query().Get("ref") == "abc123":
			if !strings.Contains(r.Header.Get("Accept"), "raw") {
				http.Error(w, "would have returned JSON, not the file", http.StatusNotAcceptable)
				return
			}
			w.Write([]byte("def ci(ctx):\n    return []\n"))
		case r.Method == "POST" && r.URL.Path == "/repos/sam/app/statuses/abc123":
			json.NewDecoder(r.Body).Decode(&gotStatus)
			w.WriteHeader(http.StatusCreated)
		default:
			http.NotFound(w, r)
		}
	}))
	defer srv.Close()
	g := forge.GitHub{Token: "tok", API: srv.URL}

	src, err := g.File(context.Background(), "sam/app", "abc123", ".ci.star")
	if err != nil || !strings.HasPrefix(string(src), "def ci") {
		t.Fatalf("File = %q, %v", src, err)
	}
	if _, err := g.File(context.Background(), "sam/app", "abc123", "missing"); !errors.Is(err, forge.ErrNotFound) {
		t.Errorf("a missing file gave %v, want ErrNotFound", err)
	}
	long := strings.Repeat("x", 300)
	if err := g.SetStatus(context.Background(), "sam/app", "abc123", forge.Status{State: "success", Description: long}); err != nil {
		t.Fatalf("SetStatus: %v", err)
	}
	if gotStatus["state"] != "success" || gotStatus["context"] != "starci" || len(gotStatus["description"]) != 140 {
		t.Errorf("status sent = %v (description length %d)", gotStatus["state"], len(gotStatus["description"]))
	}
}

func TestOutcome(t *testing.T) {
	r := func(name, status string) plan.Result { return plan.Result{Name: name, Status: status} }
	cases := []struct {
		results []plan.Result
		runErr  string
		want    string
	}{
		{[]plan.Result{r("a", "passed"), r("b", "passed")}, "", "success: 2 step(s) passed"},
		{[]plan.Result{r("a", "passed"), r("b", "failed"), r("c", "skipped")}, "", "failure: failed: b"},
		{[]plan.Result{r("a", "passed")}, "Traceback\n  ci.star:3\nError in fail: gave up", "failure: Error in fail: gave up"},
		{[]plan.Result{r("a", "passed"), r("c", "skipped")}, "", "failure: 1 step(s) skipped"},
	}
	for _, c := range cases {
		got := Outcome(c.results, c.runErr)
		if s := got.State + ": " + got.Description; s != c.want {
			t.Errorf("Outcome = %q, want %q", s, c.want)
		}
	}
}

func prBody(action string, number int, headRepo string) []byte {
	b, _ := json.Marshal(map[string]any{
		"action": action, "number": number,
		"repository": map[string]string{"full_name": "sam/app", "clone_url": "https://git.example/sam/app.git"},
		"pull_request": map[string]any{
			"head": map[string]any{"sha": "abc123", "ref": "fix-login", "repo": map[string]string{"full_name": headRepo}},
			"base": map[string]string{"ref": "main"},
		},
	})
	return b
}

const eventScript = `
def ci(ctx):
    name = ctx.event
    if ctx.event == "tag":
        name = "release " + ctx.tag
    if ctx.pr:
        name = "pr {} {} -> {}".format(ctx.pr.number, ctx.pr.head, ctx.pr.base)
    return [step(name, image = "i", run = "r")]
`

func TestATagStartsARun(t *testing.T) {
	h, fake, sub := setup(t, eventScript)
	body := pushBody("refs/tags/v1.2.0", "abc123")
	if rec := deliver(h, "push", body, sign(body, secret)); rec.Code != http.StatusAccepted {
		t.Fatalf("status %d: %s", rec.Code, rec.Body)
	}
	if len(sub.runs) != 1 {
		t.Fatalf("%d runs, want 1", len(sub.runs))
	}
	run := sub.runs[0]
	if run.Plan.Steps[0].Name != "release v1.2.0" || run.Context.Tag != "v1.2.0" || run.Context.Ref != "" {
		t.Errorf("step %q, context %+v", run.Plan.Steps[0].Name, run.Context)
	}
	// Each tag is its own line of runs, and its status its own line on the commit.
	if run.Supersedes != "sam/app@tag-v1.2.0" || run.Report.Context != "starci/tag" || fake.statuses[0]["context"] != "starci/tag" {
		t.Errorf("supersedes %q, report %+v, status context %q", run.Supersedes, run.Report, fake.statuses[0]["context"])
	}
}

func TestAPullRequestStartsARun(t *testing.T) {
	for _, action := range []string{"opened", "reopened", "synchronize", "synchronized"} {
		h, fake, sub := setup(t, eventScript)
		body := prBody(action, 7, "sam/app")
		if rec := deliver(h, "pull_request", body, sign(body, secret)); rec.Code != http.StatusAccepted {
			t.Fatalf("%s: status %d: %s", action, rec.Code, rec.Body)
		}
		if len(sub.runs) != 1 {
			t.Fatalf("%s: %d runs, want 1", action, len(sub.runs))
		}
		run := sub.runs[0]
		if run.Plan.Steps[0].Name != "pr 7 fix-login -> main" || run.Context.SHA != "abc123" || run.Context.PR.Fork {
			t.Errorf("%s: step %q, context %+v", action, run.Plan.Steps[0].Name, run.Context)
		}
		// Not the branch's line: the push to that branch has a run of its own.
		if run.Supersedes != "sam/app@pr-7" || fake.statuses[0]["context"] != "starci/pull-request" {
			t.Errorf("%s: supersedes %q, status context %q", action, run.Supersedes, fake.statuses[0]["context"])
		}
	}
}

func TestPullRequestsFromForksAreNotRunUnlessAllowed(t *testing.T) {
	h, fake, sub := setup(t, eventScript)
	body := prBody("opened", 9, "stranger/app")
	rec := deliver(h, "pull_request", body, sign(body, secret))
	if rec.Code != http.StatusAccepted || h.said != "ignored: pull requests from forks are not run" {
		t.Errorf("status %d, receiver said %q", rec.Code, h.said)
	}
	// Nothing at all happens: no script is fetched, no status is set.
	if len(sub.runs) != 0 || len(fake.statuses) != 0 {
		t.Errorf("a fork's pull request had an effect: %d runs, %d statuses", len(sub.runs), len(fake.statuses))
	}

	h.recv.AllowForks = true
	if rec := deliver(h, "pull_request", body, sign(body, secret)); rec.Code != http.StatusAccepted {
		t.Fatalf("with forks allowed: status %d: %s", rec.Code, rec.Body)
	}
	if len(sub.runs) != 1 || !sub.runs[0].Context.PR.Fork {
		t.Errorf("with forks allowed: runs = %+v", sub.runs)
	}

	// A pull request that does not say where it comes from is a stranger's.
	h2, _, sub2 := setup(t, eventScript)
	body = prBody("opened", 10, "")
	deliver(h2, "pull_request", body, sign(body, secret))
	if len(sub2.runs) != 0 {
		t.Error("a pull request naming no head repository was run")
	}
}

func TestSupersedes(t *testing.T) {
	cases := []struct {
		c    plan.Context
		want string
	}{
		{plan.Context{Repo: "a/b", SHA: "abc", Ref: "main"}, "a/b@main"},
		{plan.Context{Repo: "a/b", SHA: "abc"}, "a/b@abc"},
		{plan.Context{Repo: "a/b", Event: plan.EventTag, Tag: "v1"}, "a/b@tag-v1"},
		{plan.Context{Repo: "a/b", Event: plan.EventPullRequest, Ref: "fix", PR: &plan.PullRequest{Number: 3}}, "a/b@pr-3"},
		{plan.Context{Repo: "a/b", Event: plan.EventSchedule, Ref: "main", Schedule: "nightly"}, "a/b@schedule-nightly"},
	}
	for _, c := range cases {
		if got := Supersedes(c.c); got != c.want {
			t.Errorf("Supersedes(%+v) = %q, want %q", c.c, got, c.want)
		}
	}
}

package step

import (
	"context"
	"strings"
	"testing"

	"git.phin.tech/sam/starci/internal/plan"
)

func TestCompareNamesWhatChanged(t *testing.T) {
	was := Ingredients{
		Image: "sha256:aaa", Arch: "amd64", Run: "r1",
		Env:      map[string]string{"KEEP": "1", "CHANGE": "1", "DROP": "1"},
		Files:    map[string]string{"same.txt": "100644 a 0", "edited.txt": "100644 a 0", "gone.txt": "100644 a 0", "mode.sh": "100644 a 0"},
		Tools:    map[string]string{"bun": "sum1 ."},
		Packages: []string{"git", "libpq-dev"},
	}
	now := Ingredients{
		Image: "sha256:bbb", Arch: "arm64", Run: "r2", Offline: true,
		Env:      map[string]string{"KEEP": "1", "CHANGE": "2", "NEW": "1"},
		Files:    map[string]string{"same.txt": "100644 a 0", "edited.txt": "100644 b 0", "new.txt": "100644 a 0", "mode.sh": "100755 a 0"},
		Tools:    map[string]string{"bun": "sum2 ."},
		Packages: []string{"curl", "git"},
	}
	got := map[string][]string{}
	for _, c := range Compare(was, now) {
		got[c.What] = append(got[c.What], c.Detail)
	}
	for what, want := range map[string]string{
		"files":        "changed: edited.txt, mode.sh|added: new.txt|removed: gone.txt",
		"env":          "changed: CHANGE|added: NEW|removed: DROP",
		"tools":        "changed: bun",
		"packages":     "added: curl|removed: libpq-dev",
		"command":      "the command is not the one that passed",
		"network":      "it now has no network, and passed with it",
		"architecture": "arm64, was amd64",
	} {
		if strings.Join(got[what], "|") != want {
			t.Errorf("%s: %q, want %q", what, strings.Join(got[what], "|"), want)
		}
	}
	if len(got["image"]) != 1 || !strings.Contains(got["image"][0], "different content under the same name") {
		t.Errorf("image: %q", got["image"])
	}
	if changes := Compare(was, was); len(changes) != 0 {
		t.Errorf("a thing compared with itself differs: %+v", changes)
	}
	// Files come first: they are the usual reason.
	if first := Compare(was, now)[0]; first.What != "files" {
		t.Errorf("the first change listed is %s", first.What)
	}
}

func TestALongListIsCounted(t *testing.T) {
	was, now := Ingredients{Files: map[string]string{}}, Ingredients{Files: map[string]string{}}
	for _, name := range strings.Fields("a b c d e f g h i j k l") {
		was.Files[name], now.Files[name] = "1", "2"
	}
	if got, want := Compare(was, now)[0].Detail, "changed: a, b, c, d, e, f, g, h and 4 more"; got != want {
		t.Errorf("%q, want %q", got, want)
	}
}

func TestARepositoryIsTheSameHoweverItsAddressIsSpelled(t *testing.T) {
	want := "sam/app"
	for _, u := range []string{
		"https://git.example.com/sam/app.git",
		"https://git.example.com/sam/app",
		"https://token@git.example.com/sam/app.git/",
		"ssh://git@git.example.com:2222/sam/app.git",
		"git@git.example.com:sam/app.git",
		// The same server by the name it has inside a network.
		"ssh://git@git.internal.example:2222/sam/app.git",
		"git@git.internal.example:sam/app",
	} {
		if got := RepoID(u); got != want {
			t.Errorf("RepoID(%q) = %q, want %q", u, got, want)
		}
	}
	if got := RepoID("/home/sam/src/app"); got != "/home/sam/src/app" {
		t.Errorf("a path became %q", got)
	}
	if RepoID("https://git.example.com/sam/app") == RepoID("https://git.example.com/sam/other") {
		t.Error("two repositories have one name")
	}
}

// A key and what is kept beside it are made from the same things in two
// places. Anything that changes a key and is not seen by Compare would be
// reported as "nothing has changed" about a step that then runs.
func TestWhateverChangesAKeyIsAChange(t *testing.T) {
	base := plan.Step{Name: "test", Run: "make test", Env: map[string]string{"MODE": "ci"}, Packages: []string{"git"}}
	img := image{id: "sha256:aaa", arch: "amd64"}
	listing := "100644 aaa 0\tsrc/a.txt\n100644 bbb 0\tsrc/b.txt"
	tools := [][]string{{"bun", "sum1", "."}}
	sum := func(listing string) string { return short(listing) }

	keyOf := func(s plan.Step, img image, listing string, tools [][]string) (string, Ingredients) {
		return key(s, img, sum(listing), tools), ingredients(s, img, listing, tools)
	}
	baseKey, baseMade := keyOf(base, img, listing, tools)

	with := func(change func(s *plan.Step)) plan.Step {
		s := base
		s.Env = map[string]string{"MODE": "ci"}
		change(&s)
		return s
	}
	for name, c := range map[string]struct {
		step    plan.Step
		img     image
		listing string
		tools   [][]string
	}{
		"a file's content":      {base, img, strings.Replace(listing, "aaa", "ccc", 1), tools},
		"a file's mode":         {base, img, strings.Replace(listing, "100644 aaa", "100755 aaa", 1), tools},
		"a file added":          {base, img, listing + "\n100644 ddd 0\tsrc/c.txt", tools},
		"a file removed":        {base, img, "100644 aaa 0\tsrc/a.txt", tools},
		"the image":             {base, image{id: "sha256:bbb", arch: "amd64"}, listing, tools},
		"the architecture":      {base, image{id: "sha256:aaa", arch: "arm64"}, listing, tools},
		"the command":           {with(func(s *plan.Step) { s.Run = "make check" }), img, listing, tools},
		"a variable's value":    {with(func(s *plan.Step) { s.Env["MODE"] = "release" }), img, listing, tools},
		"a variable added":      {with(func(s *plan.Step) { s.Env["DEBUG"] = "1" }), img, listing, tools},
		"a variable removed":    {with(func(s *plan.Step) { delete(s.Env, "MODE") }), img, listing, tools},
		"a tool's checksum":     {base, img, listing, [][]string{{"bun", "sum2", "."}}},
		"a tool's bin":          {base, img, listing, [][]string{{"bun", "sum1", "bin"}}},
		"a tool added":          {base, img, listing, append([][]string{{"uv", "sum", "."}}, tools...)},
		"a package":             {with(func(s *plan.Step) { s.Packages = []string{"curl", "git"} }), img, listing, tools},
		"giving up the network": {with(func(s *plan.Step) { s.Offline = true }), img, listing, tools},
	} {
		k, made := keyOf(c.step, c.img, c.listing, c.tools)
		if k == baseKey {
			t.Errorf("%s: the key did not change, so this is not a case", name)
		}
		if len(Compare(baseMade, made)) == 0 {
			t.Errorf("%s: changes the key, and Compare sees nothing", name)
		}
	}

	// And what does not change a key is not a change.
	renamed := with(func(s *plan.Step) { s.Name = "another name" })
	if k, made := keyOf(renamed, img, listing, tools); k != baseKey || len(Compare(baseMade, made)) != 0 {
		t.Error("renaming a step changed its key, or is called a change")
	}
}

func TestWhySaysWhatChangedSinceAStepLastPassed(t *testing.T) {
	needDocker(t)
	r := newRepo(t)
	first := r.commit(map[string]string{"src/a.txt": "a", "src/b.txt": "b", "README": "x"})
	cfg := Config{Worker: "one", StateDir: t.TempDir()}
	s := plan.Step{Name: "test", Image: testImage, Inputs: []string{"src"}, Env: map[string]string{"MODE": "ci"}, Cache: true, Run: "cat src/a.txt"}
	ctx := context.Background()

	// Before it has ever passed.
	why, err := Why(ctx, cfg, r.dir, first, s)
	if err != nil || why.Known {
		t.Fatalf("before any pass: %+v, %v", why, err)
	}

	if got := run(t, cfg, r, first, s); got.Status != "passed" {
		t.Fatalf("%+v", got)
	}
	why, err = Why(ctx, cfg, r.dir, first, s)
	if err != nil || !why.Known || len(why.Changes) != 0 || !why.Stored || why.Last.SHA != first || why.Last.Worker != "one" {
		t.Fatalf("right after passing: %+v, %v; want nothing changed", why, err)
	}

	// A commit that touches nothing the step reads changes nothing.
	second := r.commit(map[string]string{"README": "y"})
	if why, _ = Why(ctx, cfg, r.dir, second, s); len(why.Changes) != 0 || !why.Stored {
		t.Errorf("after a change to a file it does not read: %+v", why.Changes)
	}

	// One that does, and a different command and variable besides.
	third := r.commit(map[string]string{"src/a.txt": "changed", "src/new.txt": "n"})
	s.Run, s.Env = "cat src/b.txt", map[string]string{"MODE": "release"}
	why, err = Why(ctx, cfg, r.dir, third, s)
	if err != nil {
		t.Fatal(err)
	}
	if got, want := Summary(why.Changes), "files changed: src/a.txt; files added: src/new.txt; command the command is not the one that passed; env changed: MODE"; got != want {
		t.Errorf("changes:\n  %s\nwant:\n  %s", got, want)
	}
	if why.Stored {
		t.Error("a step whose key changed is said to have a stored result")
	}

	// The same is said, in a line, when the step is run and not skipped.
	var logged []string
	Run(ctx, cfg, r.dir, third, s, func(line string) { logged = append(logged, line) })
	if all := strings.Join(logged, "\n"); !strings.Contains(all, "not skipped: since this last passed (on one, at "+first[:8]+"), files changed: src/a.txt") {
		t.Errorf("the run did not say why it was not skipped:\n%s", all)
	}

	// The last pass is found under any of the repository's addresses.
	if _, ok := Recall(ctx, cfg.store(), "test", "https://elsewhere.example/x/y.git", r.dir); !ok {
		t.Error("the last pass was not found among several addresses")
	}
	if _, ok := Recall(ctx, cfg.store(), "test", "https://elsewhere.example/x/y.git"); ok {
		t.Error("a pass was found under another repository's address")
	}
	if _, ok := Recall(ctx, cfg.store(), "another step", r.dir); ok {
		t.Error("a pass was found under another step's name")
	}
}

func TestAResultThatIsGoneIsNotCalledAChange(t *testing.T) {
	needDocker(t)
	r := newRepo(t)
	sha := r.commit(map[string]string{"a.txt": "a"})
	// A pass known to the shared store, asked about from a machine that has
	// only the note and not the result.
	shared := &memStore{}
	s := plan.Step{Name: "test", Image: testImage, Inputs: []string{"."}, Cache: true, Run: "true"}
	run(t, Config{Worker: "worker", StateDir: t.TempDir(), Shared: shared}, r, sha, s)
	shared.mu.Lock()
	shared.results = nil
	shared.mu.Unlock()

	why, err := Why(context.Background(), Config{Worker: "laptop", StateDir: t.TempDir(), Shared: shared}, r.dir, sha, s)
	if err != nil || !why.Known || why.Last.Worker != "worker" || len(why.Changes) != 0 || why.Stored {
		t.Errorf("%+v, %v; want the worker's pass, no changes, and no stored result", why, err)
	}
}

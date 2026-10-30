package compile

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"io/fs"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"testing"

	"github.com/minio/minio-go/v7"
	"github.com/minio/minio-go/v7/pkg/credentials"
	"go.yaml.in/yaml/v3"

	"git.phin.tech/sam/starci/internal/plan"
)

// A stand-in for GitHub's runner, enough to carry out a compiled workflow of
// bare steps here: each run step in bash, with checkout, the cache and
// artifacts done with directories. What the workflow's shell does is then
// tested by doing it, not by reading it.

type ghWorkflow struct {
	Jobs map[string]ghJob `yaml:"jobs"`
}

type ghJob struct {
	Needs   []string          `yaml:"needs"`
	Outputs map[string]string `yaml:"outputs"`
	Steps   []ghStep          `yaml:"steps"`
}

type ghStep struct {
	ID   string            `yaml:"id"`
	Name string            `yaml:"name"`
	If   string            `yaml:"if"`
	Uses string            `yaml:"uses"`
	With map[string]string `yaml:"with"`
	Env  map[string]string `yaml:"env"`
	Run  string            `yaml:"run"`
}

// runner keeps what outlives one run of a workflow: the cache.
type runner struct {
	t     *testing.T
	repo  string
	cache string
}

var expression = regexp.MustCompile(`\$\{\{\s*(.*?)\s*\}\}`)

// run carries out the workflow once, and returns each job's outputs.
func (r *runner) run(workflow string) map[string]map[string]string {
	t := r.t
	t.Helper()
	var wf ghWorkflow
	if err := yaml.Unmarshal([]byte(workflow), &wf); err != nil {
		t.Fatal(err)
	}
	artifacts := t.TempDir()
	outputs := map[string]map[string]string{}
	done := map[string]bool{}
	for len(done) < len(wf.Jobs) {
		progressed := false
		for _, id := range sortedJobs(wf.Jobs) {
			job := wf.Jobs[id]
			ready := !done[id]
			for _, n := range job.Needs {
				ready = ready && done[n]
			}
			if !ready {
				continue
			}
			outputs[id] = r.job(id, job, outputs, artifacts)
			done[id], progressed = true, true
		}
		if !progressed {
			t.Fatal("jobs wait on each other")
		}
	}
	return outputs
}

func sortedJobs(jobs map[string]ghJob) []string {
	var ids []string
	for id := range jobs {
		ids = append(ids, id)
	}
	sort.Strings(ids)
	return ids
}

func (r *runner) job(id string, job ghJob, needs map[string]map[string]string, artifacts string) map[string]string {
	t := r.t
	t.Helper()
	workspace, temp := t.TempDir(), t.TempDir()
	steps := map[string]map[string]string{}
	eval := func(s string) string {
		return expression.ReplaceAllStringFunc(s, func(m string) string {
			e := expression.FindStringSubmatch(m)[1]
			switch {
			case e == "runner.temp":
				return temp
			case strings.HasPrefix(e, "hashFiles("):
				return treeHash(t, r.repo)
			case strings.HasPrefix(e, "needs."):
				parts := strings.Split(e, ".") // needs.<job>.outputs.<name>
				return needs[parts[1]][parts[3]]
			case strings.HasPrefix(e, "steps."):
				parts := strings.Split(e, ".") // steps.<id>.outputs.<name>
				return steps[parts[1]][parts[3]]
			}
			t.Fatalf("job %s: no stand-in for ${{ %s }}", id, e)
			return ""
		})
	}
	hit := false
	var save func()
	for _, st := range job.Steps {
		switch st.If {
		case "":
		case "steps.passed.outputs.cache-hit != 'true'":
			if hit {
				continue
			}
		default:
			t.Fatalf("job %s: no stand-in for if: %s", id, st.If)
		}
		switch {
		case strings.HasPrefix(st.Uses, "actions/checkout@"):
			copyTree(t, r.repo, workspace)
		case strings.HasPrefix(st.Uses, "actions/cache@"):
			key := eval(st.With["key"])
			paths := strings.Fields(eval(st.With["path"]))
			if paths[0] == "|" {
				paths = paths[1:]
			}
			stored := filepath.Join(r.cache, fmt.Sprintf("%x", sha256.Sum256([]byte(key))))
			if _, err := os.Stat(stored); err == nil {
				hit = true
				for i, p := range paths {
					copyTree(t, filepath.Join(stored, fmt.Sprint(i)), p)
				}
			} else {
				// Saved when the job ends well, as the action's own post step does.
				save = func() {
					for i, p := range paths {
						if _, err := os.Stat(p); err == nil {
							copyTree(t, p, filepath.Join(stored, fmt.Sprint(i)))
						}
					}
				}
			}
			steps[st.ID] = map[string]string{"cache-hit": fmt.Sprint(hit)}
		case strings.HasPrefix(st.Uses, "actions/upload-artifact@"):
			from := eval(st.With["path"])
			to := filepath.Join(artifacts, st.With["name"])
			copyTree(t, filepath.Dir(from), to)
		case strings.HasPrefix(st.Uses, "actions/download-artifact@"):
			copyTree(t, filepath.Join(artifacts, st.With["name"]), eval(st.With["path"]))
		case st.Uses != "":
			t.Fatalf("job %s: no stand-in for %s", id, st.Uses)
		default:
			out := filepath.Join(temp, "github-output")
			os.WriteFile(out, nil, 0o644)
			cmd := exec.Command("bash", "-e", "-c", eval(st.Run))
			cmd.Dir = workspace
			cmd.Env = append(os.Environ(), "RUNNER_TEMP="+temp, "GITHUB_OUTPUT="+out)
			for k, v := range st.Env {
				cmd.Env = append(cmd.Env, k+"="+v)
			}
			if b, err := cmd.CombinedOutput(); err != nil {
				t.Fatalf("job %s, step %q: %v\n%s", id, st.Name, err, b)
			}
			if st.ID != "" {
				steps[st.ID] = map[string]string{}
				data, _ := os.ReadFile(out)
				for _, line := range strings.Split(strings.TrimSpace(string(data)), "\n") {
					if k, v, ok := strings.Cut(line, "="); ok {
						steps[st.ID][k] = v
					}
				}
			}
		}
	}
	if save != nil {
		save()
	}
	outputs := map[string]string{}
	for k, v := range job.Outputs {
		outputs[k] = eval(v)
	}
	return outputs
}

func copyTree(t *testing.T, from, to string) {
	t.Helper()
	if err := os.MkdirAll(to, 0o755); err != nil {
		t.Fatal(err)
	}
	if out, err := exec.Command("cp", "-R", from+"/.", to).CombinedOutput(); err != nil {
		t.Fatalf("cp %s %s: %v\n%s", from, to, err, out)
	}
}

func treeHash(t *testing.T, dir string) string {
	h := sha256.New()
	filepath.WalkDir(dir, func(p string, d fs.DirEntry, err error) error {
		if err != nil || d.IsDir() {
			return err
		}
		data, _ := os.ReadFile(p)
		fmt.Fprintf(h, "%s %x\n", p[len(dir):], sha256.Sum256(data))
		return nil
	})
	return hex.EncodeToString(h.Sum(nil))[:16]
}

// A compiled workflow hands on what a job keeps, executable, to the job
// after it, and still does when the job that kept it is skipped because it
// passed before.
func TestACompiledWorkflowHandsOnWhatAJobKept(t *testing.T) {
	if _, err := exec.LookPath("sha256sum"); err != nil {
		t.Skip("sha256sum is not here")
	}
	repo := t.TempDir()
	if err := os.WriteFile(filepath.Join(repo, "a.txt"), []byte("a\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	log := filepath.Join(t.TempDir(), "log")
	p, err := plan.Eval("ci.star", []byte(`
def ci(ctx):
    build = step("build", cache = True, keep = ["out/app"],
                 run = "mkdir -p out && printf '#!/bin/sh\\necho made\\n' > out/app && chmod +x out/app && echo built >> `+log+`")
    return [build, step("use", artifacts = {"bin/app": artifact(build, "out/app")},
                        run = "test \"$(./bin/app)\" = made && echo used >> `+log+`")]
`), plan.Context{})
	if err != nil {
		t.Fatal(err)
	}
	workflow, err := GitHub(p, Options{})
	if err != nil {
		t.Fatal(err)
	}
	r := &runner{t: t, repo: repo, cache: t.TempDir()}

	first := r.run(workflow)
	second := r.run(workflow)
	data, _ := os.ReadFile(log)
	if got := strings.Fields(string(data)); strings.Join(got, " ") != "built used used" {
		t.Errorf("the steps ran as %q; want build once, and use both times, handed the binary the second time by a build that was skipped", got)
	}
	if first["build"]["kept-1"] == "" || first["build"]["kept-1"] != second["build"]["kept-1"] {
		t.Errorf("build said it kept %q, then %q; want the same hash both times", first["build"]["kept-1"], second["build"]["kept-1"])
	}
}

// A stand-in for CircleCI, the same way: each machine a fresh home and /tmp,
// the cache and the workspace directories, and `circleci-agent step halt` a
// program that says the job is to stop after this step.
type circleRunner struct {
	t     *testing.T
	repo  string
	cache string
}

func (r *circleRunner) run(config string) {
	t := r.t
	t.Helper()
	var c struct {
		Jobs      map[string]struct{ Steps []any } `yaml:"jobs"`
		Workflows struct {
			CI struct{ Jobs []any } `yaml:"ci"`
		} `yaml:"workflows"`
	}
	if err := yaml.Unmarshal([]byte(config), &c); err != nil {
		t.Fatal(err)
	}
	// In the order the workflow lists them, which puts a job after what it
	// requires.
	workspace := t.TempDir()
	for _, j := range c.Workflows.CI.Jobs {
		id, _ := j.(string)
		if m, ok := j.(map[string]any); ok {
			for k := range m {
				id = k
			}
		}
		r.job(id, c.Jobs[id].Steps, workspace)
	}
}

func (r *circleRunner) job(id string, steps []any, workspace string) {
	t := r.t
	t.Helper()
	home, tmp, project, bin := t.TempDir(), t.TempDir(), t.TempDir(), t.TempDir()
	halted := filepath.Join(tmp, "halted")
	os.WriteFile(filepath.Join(bin, "circleci-agent"), []byte("#!/bin/sh\ntouch "+halted+"\n"), 0o755)
	place := func(s string) string {
		return strings.NewReplacer("~/", home+"/", "/tmp/", tmp+"/").Replace(s)
	}
	checksum := regexp.MustCompile(`\{\{ checksum "([^"]+)" \}\}`)
	key := func(k string) string {
		return checksum.ReplaceAllStringFunc(place(k), func(m string) string {
			data, err := os.ReadFile(checksum.FindStringSubmatch(m)[1])
			if err != nil {
				t.Fatalf("job %s: checksum of a file that is not there: %v", id, err)
			}
			return fmt.Sprintf("%x", sha256.Sum256(data))
		})
	}
	for _, raw := range steps {
		if raw == "checkout" {
			copyTree(t, r.repo, project)
			for _, args := range [][]string{{"init", "-q"}, {"add", "-A"}, {"-c", "user.name=t", "-c", "user.email=t@t", "commit", "-qm", "x"}} {
				if out, err := exec.Command("git", append([]string{"-C", project}, args...)...).CombinedOutput(); err != nil {
					t.Fatalf("git: %v\n%s", err, out)
				}
			}
			continue
		}
		for kind, body := range raw.(map[string]any) {
			b := body.(map[string]any)
			switch kind {
			case "run":
				cmd := exec.Command("bash", "-e", "-c", place(b["command"].(string)))
				cmd.Dir = project
				cmd.Env = append(os.Environ(), "HOME="+home, "PATH="+bin+":"+os.Getenv("PATH"))
				if env, ok := b["environment"].(map[string]any); ok {
					for k, v := range env {
						cmd.Env = append(cmd.Env, k+"="+v.(string))
					}
				}
				if out, err := cmd.CombinedOutput(); err != nil {
					t.Fatalf("job %s, step %q: %v\n%s", id, b["name"], err, out)
				}
			case "restore_cache":
				for _, k := range b["keys"].([]any) {
					stored := filepath.Join(r.cache, fmt.Sprintf("%x", sha256.Sum256([]byte(key(k.(string))))))
					if list, err := os.ReadFile(filepath.Join(stored, "paths")); err == nil {
						for i, p := range strings.Fields(string(list)) {
							copyTree(t, filepath.Join(stored, fmt.Sprint(i)), place(p))
						}
						break
					}
				}
			case "save_cache":
				stored := filepath.Join(r.cache, fmt.Sprintf("%x", sha256.Sum256([]byte(key(b["key"].(string))))))
				if _, err := os.Stat(stored); err == nil {
					continue // a key is saved once
				}
				var saved []string
				for i, p := range b["paths"].([]any) {
					from := place(p.(string))
					copyTree(t, from, filepath.Join(stored, fmt.Sprint(i)))
					saved = append(saved, p.(string))
				}
				os.WriteFile(filepath.Join(stored, "paths"), []byte(strings.Join(saved, "\n")), 0o644)
			case "persist_to_workspace":
				root := place(b["root"].(string))
				for _, p := range b["paths"].([]any) {
					to := filepath.Join(workspace, p.(string))
					if _, err := os.Stat(to); err == nil {
						t.Fatalf("job %s: %s is persisted twice", id, p)
					}
					copyTree(t, filepath.Join(root, p.(string)), to)
				}
			case "attach_workspace":
				copyTree(t, workspace, place(b["at"].(string)))
			default:
				t.Fatalf("job %s: no stand-in for %s", id, kind)
			}
		}
		if _, err := os.Stat(halted); err == nil {
			return
		}
	}
}

func TestACompiledCircleCIConfigurationHandsOnWhatAJobKept(t *testing.T) {
	if _, err := exec.LookPath("sha256sum"); err != nil {
		t.Skip("sha256sum is not here")
	}
	repo := t.TempDir()
	if err := os.WriteFile(filepath.Join(repo, "a.txt"), []byte("a\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	log := filepath.Join(t.TempDir(), "log")
	p, err := plan.Eval("ci.star", []byte(`
def ci(ctx):
    build = step("build", cache = True, keep = ["out"],
                 run = "mkdir -p out/lib && printf '#!/bin/sh\\necho made\\n' > out/app && chmod +x out/app && echo x > out/lib/x && echo built >> `+log+`")
    use = step("use", artifacts = {"bin": artifact(build, "out")},
               run = "test \"$(./bin/app)\" = made && test -f bin/lib/x && echo used >> `+log+`")
    again = step("again", cache = True, artifacts = {"bin": artifact(build, "out")}, run = "echo again >> `+log+`")
    return [build, use, again]
`), plan.Context{})
	if err != nil {
		t.Fatal(err)
	}
	config, err := CircleCI(p, Options{})
	if err != nil {
		t.Fatal(err)
	}
	r := &circleRunner{t: t, repo: repo, cache: t.TempDir()}
	r.run(config)
	r.run(config)
	data, _ := os.ReadFile(log)
	// The second time build is skipped and hands on what it kept before;
	// again, keyed on that, is skipped too; use, never reused, runs.
	if got := strings.Join(strings.Fields(string(data)), " "); got != "built used again used" {
		t.Errorf("the steps ran as %q, want %q", got, "built used again used")
	}
}

// A stand-in for Argo, the same way: each task a pod played by processes
// here, its init container the real starci (`starci pod prepare`) and its main
// container the template's script in bash, the image's own paths moved into
// directories of the test's, and Argo's parameters substituted. The pods share
// nothing but a real store, which STARCI_TEST_S3 names.
func TestACompiledArgoWorkflowHandsOnWhatATaskKept(t *testing.T) {
	location := os.Getenv("STARCI_TEST_S3")
	if location == "" {
		t.Skip("STARCI_TEST_S3 is not set: Argo's pods share files only through a store (docs/development.md)")
	}
	starci := filepath.Join(t.TempDir(), "starci")
	if out, err := exec.Command("go", "build", "-o", starci, "../../cmd/starci").CombinedOutput(); err != nil {
		t.Fatalf("go build: %v\n%s", err, out)
	}
	repo := t.TempDir()
	os.WriteFile(filepath.Join(repo, "a.txt"), []byte("a\n"), 0o644)
	for _, args := range [][]string{{"init", "-q"}, {"add", "-A"}, {"-c", "user.name=t", "-c", "user.email=t@t", "commit", "-qm", "x"}} {
		if out, err := exec.Command("git", append([]string{"-C", repo}, args...)...).CombinedOutput(); err != nil {
			t.Fatalf("git: %v\n%s", err, out)
		}
	}
	sha, _ := exec.Command("git", "-C", repo, "rev-parse", "HEAD").Output()
	log := filepath.Join(t.TempDir(), "log")
	makeBucket(t, location)
	// A prefix of its own, so that nothing an earlier run stored is found.
	store := strings.Replace(location, "?", fmt.Sprintf("/argo-%d?", os.Getpid()), 1)
	if !strings.Contains(store, "?") {
		store += fmt.Sprintf("/argo-%d", os.Getpid())
	}
	src := []byte(`
def ci(ctx):
    build = step("build", image = "alpine:3", keep = ["out"],
                 run = "mkdir -p out && printf '#!/bin/sh\\necho made\\n' > out/app && chmod +x out/app && echo built >> ` + log + `")
    return [build, step("use", image = "alpine:3", cache = False, artifacts = {"bin": artifact(build, "out")},
                        run = "test \"$(./bin/app)\" = made && echo used >> ` + log + `")]
`)
	out, err := Argo("ci.star", src, Options{InitImage: "init"})
	if err != nil {
		t.Fatal(err)
	}
	var wf struct {
		Spec struct {
			Templates []struct {
				DAG struct {
					Tasks []struct {
						Name      string
						Arguments struct {
							Parameters []struct{ Name, Value string }
						}
						Inline struct {
							InitContainers []struct {
								Command []string
								Env     []struct{ Name, Value string }
							} `yaml:"initContainers"`
							Container struct {
								Command []string
								Args    []string
								Env     []struct{ Name, Value string }
							}
							Outputs struct {
								Parameters []struct {
									Name      string
									ValueFrom struct{ Path string } `yaml:"valueFrom"`
								}
							}
						}
					}
				}
			}
		}
	}
	if err := yaml.Unmarshal([]byte(out), &wf); err != nil {
		t.Fatal(err)
	}
	params := map[string]string{"clone_url": repo, "sha": strings.TrimSpace(string(sha)), "store": store}

	runOnce := func() {
		outputs := map[string]string{}
		for _, task := range wf.Spec.Templates[0].DAG.Tasks {
			root := t.TempDir()
			workspace, shared, tmp := filepath.Join(root, "workspace"), filepath.Join(root, "opt-starci"), filepath.Join(root, "tmp")
			os.MkdirAll(workspace, 0o755)
			os.MkdirAll(tmp, 0o755)
			args := map[string]string{}
			for _, a := range task.Arguments.Parameters {
				args[a.Name] = taskOutput.ReplaceAllStringFunc(a.Value, func(m string) string {
					return outputs[taskOutput.FindStringSubmatch(m)[1]]
				})
			}
			move := strings.NewReplacer("/opt/starci", shared, "/workspace", workspace, "/tmp/", tmp+"/")
			sub := regexp.MustCompile(`\{\{(workflow|inputs)\.parameters\.([a-z_]+)\}\}`)
			value := func(v string) string {
				return move.Replace(sub.ReplaceAllStringFunc(v, func(m string) string {
					g := sub.FindStringSubmatch(m)
					if g[1] == "workflow" {
						return params[g[2]]
					}
					return args[g[2]]
				}))
			}
			environ := func(env []struct{ Name, Value string }) []string {
				e := append(os.Environ(), "AWS_ACCESS_KEY_ID=test", "AWS_SECRET_ACCESS_KEY=test")
				for _, kv := range env {
					v := kv.Value
					if kv.Name != "STARCI_STEP" && kv.Name != "STARCI_GIVEN" {
						v = value(v)
					} else {
						v = sub.ReplaceAllStringFunc(v, func(m string) string { return args[sub.FindStringSubmatch(m)[2]] })
					}
					e = append(e, kv.Name+"="+v)
				}
				return e
			}
			init := task.Inline.InitContainers[0]
			cmd := exec.Command(starci, append(init.Command[1:], "--workspace", workspace, "--shared", shared)...)
			cmd.Env = environ(init.Env)
			if b, err := cmd.CombinedOutput(); err != nil {
				t.Fatalf("task %s, init: %v\n%s", task.Name, err, b)
			}
			c := task.Inline.Container
			script := strings.Join(c.Args, " ")
			script = strings.ReplaceAll(script, "/opt/starci/starci pod finish", "/opt/starci/starci pod finish --workspace /workspace --shared /opt/starci --out /tmp/starci-kept.json")
			main := exec.Command("bash", "-c", move.Replace(script))
			if c.Command[0] != "sh" {
				main = exec.Command(c.Command[0], c.Command[1:]...)
			}
			main.Dir = workspace
			main.Env = environ(c.Env)
			if b, err := main.CombinedOutput(); err != nil {
				t.Fatalf("task %s, main: %v\n%s", task.Name, err, b)
			}
			for _, p := range task.Inline.Outputs.Parameters {
				data, err := os.ReadFile(move.Replace(p.ValueFrom.Path))
				if err != nil {
					data = []byte("{}")
				}
				outputs[task.Name] = string(data)
			}
		}
	}
	runOnce()
	runOnce()
	data, _ := os.ReadFile(log)
	if got := strings.Join(strings.Fields(string(data)), " "); got != "built used used" {
		t.Errorf("the steps ran as %q; want build once, and use both times, handed the binary the second time by a build that passed before", got)
	}
}

// taskOutput is how an Argo task names what a task before it left.
var taskOutput = regexp.MustCompile(`\{\{tasks\.([a-z0-9-]+)\.outputs\.parameters\.kept\}\}`)

// makeBucket makes the bucket an s3:// location names, if it is missing, as
// the store's own tests do.
func makeBucket(t *testing.T, location string) {
	t.Helper()
	u, err := url.Parse(location)
	if err != nil {
		t.Fatal(err)
	}
	client, err := minio.New(u.Query().Get("endpoint"), &minio.Options{
		Creds:  credentials.NewStaticV4("test", "test", ""),
		Secure: u.Query().Get("insecure") == "",
		Region: u.Query().Get("region"),
	})
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	if ok, err := client.BucketExists(ctx, u.Host); err != nil {
		t.Fatal(err)
	} else if !ok {
		if err := client.MakeBucket(ctx, u.Host, minio.MakeBucketOptions{Region: u.Query().Get("region")}); err != nil {
			t.Fatal(err)
		}
	}
}

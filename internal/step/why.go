package step

import (
	"bytes"
	"compress/gzip"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"net/url"
	"os"
	"sort"
	"strings"
	"time"

	"git.phin.tech/sam/starci/internal/plan"
)

// Ingredients is what a step's key is made of, kept apart so that two of
// them can be compared. A key is one checksum over all of it, and two keys
// that differ say only that something did.
//
// It holds nothing a key does not, and less: a command and the values of
// environment variables are kept as checksums, since all that is ever asked
// of them is whether they changed.
type Ingredients struct {
	// Version is the version of the key itself. When what goes into a key
	// changes meaning, every step runs again, and that is the reason.
	Version int `json:"version"`
	// Image is the content id of the step's image; Arch what it runs on.
	Image string `json:"image"`
	Arch  string `json:"arch"`
	// Run is a checksum of the command.
	Run string `json:"run"`
	// Env maps each variable to a checksum of its value.
	Env map[string]string `json:"env,omitempty"`
	// Files maps each file the inputs select to its mode and content id.
	Files map[string]string `json:"files,omitempty"`
	// Tools maps each tool to its checksum and the directories put on PATH.
	Tools    map[string]string `json:"tools,omitempty"`
	Packages []string          `json:"packages,omitempty"`
	Offline  bool              `json:"offline,omitempty"`
}

// LastPass is the last time a step passed, as far as a store knows: when,
// where, at which commit, and what its key was made of then.
type LastPass struct {
	Key    string    `json:"key"`
	SHA    string    `json:"sha"`
	Worker string    `json:"worker"`
	When   time.Time `json:"when"`
	Ingredients
}

func short(s string) string {
	sum := sha256.Sum256([]byte(s))
	return hex.EncodeToString(sum[:8])
}

// ingredients gathers what key hashes, from the same values.
func ingredients(s plan.Step, img image, listing string, tools [][]string) Ingredients {
	in := Ingredients{Version: keyVersion, Image: img.id, Arch: img.arch, Run: short(s.Run), Packages: s.Packages, Offline: s.Offline,
		Env: map[string]string{}, Files: map[string]string{}, Tools: map[string]string{}}
	for k, v := range s.Env {
		in.Env[k] = short(v)
	}
	// One line for each file: its mode, content id and stage, a tab, its path.
	for _, line := range strings.Split(listing, "\n") {
		if meta, path, ok := strings.Cut(line, "\t"); ok {
			in.Files[path] = meta
		}
	}
	for _, t := range tools {
		if len(t) > 0 {
			in.Tools[t[0]] = strings.Join(t[1:], " ")
		}
	}
	return in
}

// Change is one way in which what a step is keyed on differs from what it
// was keyed on when it last passed.
type Change struct {
	// What is the kind of thing: files, image, command, env, tools,
	// packages, network or architecture.
	What string
	// Detail says which, in words.
	Detail string
}

// most is how many names are listed before the rest are counted.
const most = 8

func listed(verb string, names []string) string {
	sort.Strings(names)
	if len(names) > most {
		return fmt.Sprintf("%s: %s and %d more", verb, strings.Join(names[:most], ", "), len(names)-most)
	}
	return verb + ": " + strings.Join(names, ", ")
}

// byName compares two maps and says, of their names, which changed, which
// were added and which removed.
func byName(what string, was, now map[string]string) []Change {
	var changed, added, removed []string
	for name, v := range now {
		if old, ok := was[name]; !ok {
			added = append(added, name)
		} else if old != v {
			changed = append(changed, name)
		}
	}
	for name := range was {
		if _, ok := now[name]; !ok {
			removed = append(removed, name)
		}
	}
	var out []Change
	for _, c := range []struct {
		verb  string
		names []string
	}{{"changed", changed}, {"added", added}, {"removed", removed}} {
		if len(c.names) > 0 {
			out = append(out, Change{What: what, Detail: listed(c.verb, c.names)})
		}
	}
	return out
}

// Compare says how now differs from was, most telling first: the files a
// commit changed are the usual reason, and an image that moved under a tag
// the usual surprise.
func Compare(was, now Ingredients) []Change {
	out := byName("files", was.Files, now.Files)
	if was.Version != now.Version {
		out = append(out, Change{"starci", "this version of starci keys results differently, so every step runs once more"})
	}
	if was.Image != now.Image {
		out = append(out, Change{"image", fmt.Sprintf("it is different content under the same name (%.19s, was %.19s)", now.Image, was.Image)})
	}
	if was.Arch != now.Arch {
		out = append(out, Change{"architecture", fmt.Sprintf("%s, was %s", now.Arch, was.Arch)})
	}
	if was.Run != now.Run {
		out = append(out, Change{"command", "the command is not the one that passed"})
	}
	out = append(out, byName("env", was.Env, now.Env)...)
	out = append(out, byName("tools", was.Tools, now.Tools)...)
	if strings.Join(was.Packages, " ") != strings.Join(now.Packages, " ") {
		asSet := func(list []string) map[string]string {
			m := map[string]string{}
			for _, p := range list {
				m[p] = ""
			}
			return m
		}
		out = append(out, byName("packages", asSet(was.Packages), asSet(now.Packages))...)
	}
	if was.Offline != now.Offline {
		detail := "it now has the network, and passed without it"
		if now.Offline {
			detail = "it now has no network, and passed with it"
		}
		out = append(out, Change{"network", detail})
	}
	return out
}

// Summary is changes in one line, for a log.
func Summary(changes []Change) string {
	var parts []string
	for _, c := range changes {
		parts = append(parts, c.What+" "+c.Detail)
	}
	return strings.Join(parts, "; ")
}

// RepoID turns the address a commit is fetched from into a name for the
// repository that is the same however the address is spelled: over HTTPS or
// SSH, with or without a user or ".git", and by whichever of its names the
// server is reached. A path on this machine stays a path.
//
// It is what a step's last pass is remembered under, with the step's name.
// The host is left out on purpose. A forge is often one machine with two
// names, one for the web and one for SSH inside a network, and a pass
// recorded by a worker that fetched from the first has to be found from a
// laptop whose remote is the second. Two repositories on different hosts
// with the same owner and name would share a record; the worst that does is
// explain one with the other's last pass, since the record is never what a
// step is skipped by.
func RepoID(cloneURL string) string {
	s := strings.TrimSuffix(strings.TrimRight(cloneURL, "/"), ".git")
	if u, err := url.Parse(s); err == nil && u.Host != "" {
		return strings.Trim(u.Path, "/")
	}
	// git@host:owner/repo, which is not a URL.
	if at := strings.Index(s, "@"); at >= 0 && !strings.Contains(s[:at], "/") {
		if host, path, ok := strings.Cut(s[at+1:], ":"); ok && !strings.Contains(host, "/") {
			return strings.Trim(path, "/")
		}
	}
	return s
}

func lastPassDoc(repoID, stepName string) string {
	sum := sha256.Sum256([]byte(repoID + "\n" + stepName))
	return "last-pass/" + hex.EncodeToString(sum[:])
}

// remember records a pass as the step's last, for the next run that is not
// skipped to be explained by. Failing to is not worth a word: the step
// passed, and all that is lost is an explanation.
func remember(ctx context.Context, results Store, cloneURL string, s plan.Step, pass LastPass) {
	data, err := json.Marshal(pass)
	if err != nil {
		return
	}
	// A repository's whole listing compresses to a fraction of itself.
	var packed bytes.Buffer
	zw := gzip.NewWriter(&packed)
	zw.Write(data)
	if zw.Close() != nil {
		return
	}
	_ = results.PutDoc(ctx, lastPassDoc(RepoID(cloneURL), s.Name), packed.Bytes())
}

// Recall returns the last pass of the step called name in the repository
// fetched from any of cloneURLs, the most recent if several are known.
func Recall(ctx context.Context, results Store, name string, cloneURLs ...string) (LastPass, bool) {
	var newest LastPass
	found := false
	for _, u := range cloneURLs {
		data, ok := results.GetDoc(ctx, lastPassDoc(RepoID(u), name))
		if !ok {
			continue
		}
		zr, err := gzip.NewReader(bytes.NewReader(data))
		if err != nil {
			continue
		}
		raw, err := io.ReadAll(zr)
		var pass LastPass
		if err != nil || json.Unmarshal(raw, &pass) != nil {
			continue
		}
		if !found || pass.When.After(newest.When) {
			newest, found = pass, true
		}
	}
	return newest, found
}

// Explanation is why a step would or would not run again.
type Explanation struct {
	// Last is the step's last pass; Known is false if there is none.
	Last  LastPass
	Known bool
	// Changes is how what the step is keyed on now differs from then.
	Changes []Change
	// Stored reports whether the result of the last pass can still be had.
	// With no changes and no stored result, the step would run all the same.
	Stored bool
}

// Why works out, without running the step, whether s would be skipped for
// the commit sha fetched from cloneURL, and if not, what has changed since
// it last passed. known are other addresses the same repository goes by,
// such as its remotes, under which a worker may have recorded that pass.
//
// It fetches the commit and identifies the image, so it needs git and
// docker. It downloads no tools: a tool is known by the checksum the script
// gives.
func Why(ctx context.Context, cfg Config, cloneURL, sha string, s plan.Step, known ...string) (Explanation, error) {
	root, err := os.MkdirTemp("", "starci-why-")
	if err != nil {
		return Explanation{}, err
	}
	defer os.RemoveAll(root)
	co, err := fetch(cloneURL, sha, root, cfg.GitEnv)
	if err != nil {
		return Explanation{}, fmt.Errorf("fetch of %s failed: %v", sha, err)
	}
	content, listing, err := co.contentID(s.Inputs)
	if err != nil {
		return Explanation{}, err
	}
	img, err := inspectImage(s.Image)
	if err != nil {
		return Explanation{}, err
	}
	toolIDs, err := toolIdentities(s.Tools, img.arch)
	if err != nil {
		return Explanation{}, err
	}
	now := ingredients(s, img, listing, toolIDs)
	results := cfg.store()

	var why Explanation
	why.Last, why.Known = Recall(ctx, results, s.Name, append([]string{cloneURL}, known...)...)
	if !why.Known {
		return why, nil
	}
	why.Changes = Compare(why.Last.Ingredients, now)
	_, why.Stored = results.Get(ctx, key(s, img, content, toolIDs))
	return why, nil
}

// toolIdentities is what identifies each of a step's tools in its key: the
// name, the checksum of the build for arch, and the directories put on PATH.
func toolIdentities(tools []plan.Tool, arch string) ([][]string, error) {
	var ids [][]string
	for _, t := range tools {
		b, ok := buildFor(t, arch)
		if !ok {
			return nil, fmt.Errorf("tool %q has no build for %s, which is what the step's image runs on", t.Name, arch)
		}
		ids = append(ids, append([]string{t.Name, b.sum}, b.bins...))
	}
	return ids, nil
}

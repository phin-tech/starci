package forge

import (
	"encoding/base64"
	"strings"
	"testing"
)

func TestGitEnvCarriesCredentialsAsConfiguration(t *testing.T) {
	env := GitEnv(Forgejo{BaseURL: "https://git.example.com/", Token: "fj-token"}, GitHub{Token: "gh-token"}, Forgejo{BaseURL: "https://other.example"})
	got := strings.Join(env, "\n")
	basic := base64.StdEncoding.EncodeToString([]byte("x-access-token:gh-token"))
	for _, want := range []string{
		"GIT_CONFIG_COUNT=2",
		"GIT_CONFIG_KEY_0=http.https://git.example.com/.extraheader",
		"GIT_CONFIG_VALUE_0=Authorization: token fj-token",
		"GIT_CONFIG_KEY_1=http.https://github.com/.extraheader",
		"GIT_CONFIG_VALUE_1=Authorization: Basic " + basic,
	} {
		if !strings.Contains(got, want) {
			t.Errorf("GitEnv is missing %q; got:\n%s", want, got)
		}
	}
	// A forge with no token asks for nothing, so a public fetch stays anonymous.
	if strings.Contains(got, "other.example") {
		t.Errorf("a forge without a token contributed configuration:\n%s", got)
	}
	if GitEnv(Forgejo{BaseURL: "https://x"}) != nil {
		t.Error("GitEnv with no tokens should be empty")
	}
}

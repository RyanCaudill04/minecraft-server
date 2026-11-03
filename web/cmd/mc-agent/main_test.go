package main

import "testing"

func TestDefaultRepo(t *testing.T) {
	defer func(old string) { builtRepo = old }(builtRepo)
	for _, tc := range []struct {
		name, env, built, want string
	}{
		{"REPO wins", "fork/server", "upstream/server", "fork/server"},
		{"falls back to the repo that built it", "", "upstream/server", "upstream/server"},
		{"none in a local build", "", "", ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Setenv("REPO", tc.env)
			builtRepo = tc.built
			if got := defaultRepo(); got != tc.want {
				t.Errorf("defaultRepo() = %q, want %q", got, tc.want)
			}
		})
	}
}

func TestValidRepo(t *testing.T) {
	for repo, want := range map[string]bool{
		"RyanCaudill04/minecraft-server": true,
		"a/b.c_d-e":                      true,
		"":                               false,
		"noslash":                        false,
		"a/b/c":                          false,
		"a /b":                           false,
		"a/.github":                      true,
		"../x":                           false,
		"a/..":                           false,
		"a/.":                            false,
	} {
		if got := validRepo.MatchString(repo); got != want {
			t.Errorf("validRepo(%q) = %v, want %v", repo, got, want)
		}
	}
}

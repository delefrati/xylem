package client

import "testing"

func TestScope(t *testing.T) {
	t.Parallel()
	s := NewScope([][]byte{[]byte("services/api"), []byte("libs")})
	trees := map[string]bool{
		"": true, "services": true, "services/api": true, "services/api/v1": true, "libs": true, "libs/x/y": true,
		"serv": false, "services/apix": false, "services/web": false, "lib": false, "other": false,
	}
	for p, want := range trees {
		if got := s.TreeInScope([]byte(p)); got != want {
			t.Errorf("TreeInScope(%q) = %v, want %v", p, got, want)
		}
	}
	files := map[string]bool{
		"README": true, "services/api/main.go": true, "services/api/v1/x.go": true, "libs/a.go": true,
		"services/README": false, "services/apix/f": false, "serv/f": false, "other/f": false,
	}
	for p, want := range files {
		if got := s.FileInScope([]byte(p)); got != want {
			t.Errorf("FileInScope(%q) = %v, want %v", p, got, want)
		}
	}
	full := NewScope(nil)
	if !full.Full() || !full.TreeInScope([]byte("anything/deep")) || !full.FileInScope([]byte("a/b/c")) {
		t.Error("full scope excludes something")
	}
}

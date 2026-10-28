package main

import (
	"net/http"
	"net/http/httptest"
	"testing"
)

// 回归 P2-3：旧实现直接 Header["Location"][0]，相对 Location 不会被解析成绝对地址。
func TestGetHttpLocationRelativeRedirect(t *testing.T) {
	mux := http.NewServeMux()
	mux.HandleFunc("/start", func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, "/real-target", http.StatusFound)
	})
	mux.HandleFunc("/real-target", func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte("ok"))
	})
	srv := httptest.NewServer(mux)
	defer srv.Close()

	got := GetHttpLocation(srv.URL + "/start")
	want := srv.URL + "/real-target"
	if got != want {
		t.Fatalf("GetHttpLocation = %q, want %q", got, want)
	}
}

func TestGetHttpLocationAbsoluteRedirect(t *testing.T) {
	mux := http.NewServeMux()
	mux.HandleFunc("/start", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Location", "https://cdn.example.com/asset.bin")
		w.WriteHeader(http.StatusMovedPermanently)
	})
	srv := httptest.NewServer(mux)
	defer srv.Close()

	got := GetHttpLocation(srv.URL + "/start")
	if got != "https://cdn.example.com/asset.bin" {
		t.Fatalf("GetHttpLocation = %q, want the absolute location", got)
	}
}

func TestGetHttpLocationNoRedirect(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte("ok"))
	}))
	defer srv.Close()

	if got := GetHttpLocation(srv.URL); got != "" {
		t.Fatalf("GetHttpLocation = %q, want empty for a non redirecting target", got)
	}
}

func TestGetHttpLocationInvalidURL(t *testing.T) {
	for _, in := range []string{"", "://nope", "http://127.0.0.1:1/x"} {
		if got := GetHttpLocation(in); got != "" {
			t.Errorf("GetHttpLocation(%q) = %q, want empty", in, got)
		}
	}
}

func TestResolveLocation(t *testing.T) {
	cases := []struct {
		base, loc, want string
	}{
		{"http://a.test/x/y", "/z", "http://a.test/z"},
		{"http://a.test/x/y", "z", "http://a.test/x/z"},
		{"http://a.test/x/y", "https://b.test/q", "https://b.test/q"},
	}
	for _, c := range cases {
		if got := resolveLocation(c.base, c.loc); got != c.want {
			t.Errorf("resolveLocation(%q, %q) = %q, want %q", c.base, c.loc, got, c.want)
		}
	}
}

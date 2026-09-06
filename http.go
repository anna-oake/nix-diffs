package main

import (
	"embed"
	"encoding/json"
	"errors"
	"io/fs"
	"log/slog"
	"net/http"
	"strings"
)

//go:embed web
var assets embed.FS

func (a *app) handler() http.Handler {
	mux := http.NewServeMux()
	static, _ := fs.Sub(assets, "web")
	files := http.StripPrefix("/static/", http.FileServerFS(static))
	mux.Handle("GET /static/{file}", files)
	mux.Handle("GET /static/fonts/{file}", files)
	mux.HandleFunc("GET /api/repos", func(w http.ResponseWriter, r *http.Request) {
		repos, err := a.catalog(r.Context())
		if err != nil {
			a.fail(w, err)
			return
		}
		writeJSON(w, 200, limitCatalog(repos))
	})
	mux.HandleFunc("GET /api/comparisons/{owner}/{repo}/{old}/{new}", a.diffAPI)
	mux.HandleFunc("GET /{owner}/{repo}/{old}/{new}", func(w http.ResponseWriter, r *http.Request) {
		if !validRoute(r) {
			http.Error(w, "Invalid comparison URL", 400)
			return
		}
		a.page(w, r)
	})
	mux.HandleFunc("GET /{$}", a.page)
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("X-Content-Type-Options", "nosniff")
		w.Header().Set("Referrer-Policy", "same-origin")
		w.Header().Set("Content-Security-Policy", "default-src 'self'; script-src 'self'; style-src 'self'; font-src 'self'; img-src 'self'; object-src 'none'; base-uri 'none'; frame-ancestors 'none'")
		if !strings.HasPrefix(r.URL.Path, "/static/") {
			w.Header().Set("Cache-Control", "no-store")
		}
		mux.ServeHTTP(w, r)
	})
}
func validRoute(r *http.Request) bool {
	return component(r.PathValue("owner")) && component(r.PathValue("repo")) && commitPattern.MatchString(r.PathValue("old")) && commitPattern.MatchString(r.PathValue("new"))
}
func (a *app) page(w http.ResponseWriter, r *http.Request) {
	data, _ := assets.ReadFile("web/index.html")
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	root, diffClass, compareHidden := "./", "", ""
	if r.PathValue("owner") != "" {
		root, diffClass, compareHidden = "../../../", " in-diff", "hidden"
	}
	w.Write([]byte(strings.NewReplacer("__ROOT__", root, "__DIFF_CLASS__", diffClass, "__COMPARE_HIDDEN__", compareHidden).Replace(string(data))))
}
func writeJSON(w http.ResponseWriter, status int, value any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(value)
}
func (a *app) fail(w http.ResponseWriter, err error) {
	slog.Error("Request failed", "error", err)
	writeJSON(w, 500, map[string]string{"error": "Could not generate the comparison. Check the service logs, then try again."})
}
func (a *app) diffAPI(w http.ResponseWriter, r *http.Request) {
	if !validRoute(r) {
		writeJSON(w, 400, map[string]string{"error": "Invalid repository or commit."})
		return
	}
	k := requestKey{Owner: r.PathValue("owner"), Repo: r.PathValue("repo"), Old: r.PathValue("old"), New: r.PathValue("new"), Attr: r.URL.Query().Get("attr")}
	old, next, err := a.orderedCommits(r.Context(), k)
	if errors.Is(err, errBackwards) {
		writeJSON(w, 400, map[string]string{"error": err.Error()})
		return
	}
	if errors.Is(err, errNotReady) {
		writeJSON(w, 202, map[string]string{"status": "not_ready", "message": "Build not ready."})
		return
	}
	if err != nil {
		a.fail(w, err)
		return
	}
	if k.Attr == "" {
		attrs, err := a.comparisonHosts(r.Context(), k)
		if err != nil {
			a.fail(w, err)
			return
		}
		all := attribute{ID: "all", Name: "All hosts", Kind: "All", Old: true, New: true}
		available, err := a.comparisonAttributes(k.Owner, k.Repo, k.Old, k.New)
		if err != nil {
			a.fail(w, err)
			return
		}
		for _, host := range available {
			if !host.Old || !host.New {
				all.Pending++
			}
		}
		key := k
		key.Attr = "all"
		entry, _, err := a.compare(r.Context(), key)
		if err == nil {
			var report struct {
				Diffs []json.RawMessage `json:"diffs"`
			}
			if json.Unmarshal(entry.Report, &report) == nil {
				count := len(report.Diffs)
				all.Changes = &count
			}
		} else if errors.Is(err, errNotReady) {
			all.Old = false
			all.New = false
		} else {
			all.CountFailed = true
			slog.Warn("Combined comparison failed", "error", err)
		}
		attrs = append([]attribute{all}, attrs...)
		writeJSON(w, 200, map[string]any{"attributes": attrs, "old": old, "new": next})
		return
	}
	if !component(k.Attr) || (k.Attr != "all" && describe(k.Attr).Kind == "Other") {
		writeJSON(w, 400, map[string]string{"error": "Invalid attribute."})
		return
	}
	entry, hit, err := a.compare(r.Context(), k)
	if errors.Is(err, errNotReady) {
		w.Header().Set("Retry-After", "15")
		writeJSON(w, 202, map[string]string{"status": "not_ready", "message": "Build not ready. One or both snapshots have not arrived yet."})
		return
	}
	if err != nil {
		a.fail(w, err)
		return
	}
	w.Header().Set("X-Diff-Cache", map[bool]string{true: "HIT", false: "MISS"}[hit])
	writeJSON(w, 200, map[string]any{"status": "ready", "cached": hit, "generated_at": entry.Generated, "report": entry.Report})
}

func limitCatalog(repos []repository) []repository {
	for i := range repos {
		commits := repos[i].Commits
		repos[i].RecentCommits = commits[:min(len(commits), 5)]
		repos[i].Commits = commits[:min(len(commits), 50)]
	}
	return repos
}

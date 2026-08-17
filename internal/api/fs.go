package api

import (
	"net/http"
	"os"
	"path/filepath"
	"sort"
	"strings"
)

// handleFSRoots returns the allowed roots for the server-side directory browser.
// The "import from directory" UI starts navigation from one of these.
func (s *Server) handleFSRoots(w http.ResponseWriter, r *http.Request) {
	roots := s.cfg.Server.EffectiveFSRoots()
	writeJSON(w, http.StatusOK, map[string]any{"roots": roots})
}

// handleFSList lists the immediate entries of a directory, restricted to the
// configured roots. Only directories are navigable; files are returned for
// display, but the UI passes a directory path as the build context. Hidden
// entries (leading ".") are skipped to keep the picker uncluttered.
func (s *Server) handleFSList(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		writeError(w, http.StatusMethodNotAllowed, "方法不被允许")
		return
	}
	dir := strings.TrimSpace(r.URL.Query().Get("dir"))
	if dir == "" {
		writeError(w, http.StatusBadRequest, "缺少 dir 参数")
		return
	}
	abs, err := filepath.Abs(dir)
	if err != nil {
		writeError(w, http.StatusBadRequest, "路径解析失败: "+err.Error())
		return
	}
	if !s.cfg.Server.FSPathAllowed(abs) {
		writeError(w, http.StatusForbidden, "该目录不在允许访问的根目录范围内")
		return
	}
	info, err := os.Stat(abs)
	if err != nil {
		writeError(w, http.StatusNotFound, "目录不存在或无法访问: "+abs)
		return
	}
	if !info.IsDir() {
		writeError(w, http.StatusBadRequest, "不是目录: "+abs)
		return
	}
	// Resolve symlinks to the real path and re-check the allow-list. A symlink
	// inside an allowed root (e.g. root/link -> /etc) passed the lexical check
	// above but would otherwise let os.ReadDir list a directory outside the
	// configured roots. Re-validating the resolved path closes that escape.
	if real, rerr := filepath.EvalSymlinks(abs); rerr == nil && real != abs {
		if !s.cfg.Server.FSPathAllowed(real) {
			writeError(w, http.StatusForbidden, "该目录不在允许访问的根目录范围内")
			return
		}
	}
	entries, err := os.ReadDir(abs)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "读取目录失败: "+err.Error())
		return
	}

	type entry struct {
		Name  string `json:"name"`
		IsDir bool   `json:"is_dir"`
		Size  int64  `json:"size"`
	}
	dirs := make([]entry, 0, len(entries))
	files := make([]entry, 0, len(entries))
	for _, e := range entries {
		name := e.Name()
		if strings.HasPrefix(name, ".") {
			continue
		}
		fi, serr := e.Info()
		if serr != nil {
			continue
		}
		en := entry{Name: name, IsDir: e.IsDir(), Size: fi.Size()}
		if e.IsDir() {
			dirs = append(dirs, en)
		} else {
			files = append(files, en)
		}
	}
	sort.Slice(dirs, func(i, j int) bool { return dirs[i].Name < dirs[j].Name })
	sort.Slice(files, func(i, j int) bool { return files[i].Name < files[j].Name })

	writeJSON(w, http.StatusOK, map[string]any{
		"dir":     abs,
		"entries": append(dirs, files...),
	})
}

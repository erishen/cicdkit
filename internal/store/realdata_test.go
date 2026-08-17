package store

import (
	"os"
	"path/filepath"
	"testing"
)

// 用真实数据文件验证：分页三页并集必须 = total，且无重叠。
func TestRealDataPaginationDisjoint(t *testing.T) {
	root, _ := os.Getwd()
	// 从 internal/store 向上两级到仓库根，再进 data/store.json
	path := filepath.Join(root, "..", "..", "data", "store.json")
	st, err := NewJsonStore(path)
	if err != nil {
		t.Fatalf("open store: %v", err)
	}

	check := func(name string, total int, get func(off int) (items []string, tot int)) {
		seen := map[string]bool{}
		dup := 0
		sum := 0
		for off := 0; off < total; off += 20 {
			items, _ := get(off)
			sum += len(items)
			for _, id := range items {
				if seen[id] {
					dup++
				}
				seen[id] = true
			}
		}
		if len(seen) != total {
			t.Errorf("%s: 去重后唯一 %d != total %d (sum=%d dup=%d)", name, len(seen), total, sum, dup)
		}
		if dup > 0 {
			t.Errorf("%s: 分页有 %d 条重叠", name, dup)
		}
	}

	dp := st.ListDeploymentsPaged("", 20, 0)
	check("deployments", dp.Total, func(off int) ([]string, int) {
		p := st.ListDeploymentsPaged("", 20, off)
		ids := make([]string, 0, len(p.Deployments))
		for _, d := range p.Deployments {
			ids = append(ids, d.ID)
		}
		return ids, p.Total
	})

	rp := st.ListRunsPaged("", 20, 0)
	check("runs", rp.Total, func(off int) ([]string, int) {
		p := st.ListRunsPaged("", 20, off)
		ids := make([]string, 0, len(p.Runs))
		for _, r := range p.Runs {
			ids = append(ids, r.ID)
		}
		return ids, p.Total
	})
}

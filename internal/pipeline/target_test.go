package pipeline

import (
	"testing"

	"github.com/erishen/cicdkit/internal/store"
)

func TestEffectiveDeployTarget(t *testing.T) {
	p := store.Project{PrimaryTarget: "阿里云"}
	if got := effectiveDeployTarget("腾讯云", p); got != "腾讯云" {
		t.Fatalf("显式目标应优先，得到 %q", got)
	}
	if got := effectiveDeployTarget("", p); got != "阿里云" {
		t.Fatalf("未显式指定时应回落到主发布目标，得到 %q", got)
	}
	if got := effectiveDeployTarget("", store.Project{}); got != "" {
		t.Fatalf("没有主目标时应为空，得到 %q", got)
	}
}

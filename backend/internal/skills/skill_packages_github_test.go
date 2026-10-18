package skills

import (
	"net/http"
	"testing"
)

// 令牌只走环境变量：装技能是低频动作，不该为它新增落库字段和管理端界面。
func TestSetGitHubAuthorizationUsesConfiguredToken(t *testing.T) {
	request, err := http.NewRequest(http.MethodGet, "https://api.github.com/repos/owner/repo", nil)
	if err != nil {
		t.Fatal(err)
	}
	t.Setenv("CANVAS_GITHUB_TOKEN", "  ghp_test  ")
	setGitHubAuthorization(request)
	if got := request.Header.Get("Authorization"); got != "Bearer ghp_test" {
		t.Fatalf("令牌未注入或未被裁剪: %q", got)
	}
}

func TestSetGitHubAuthorizationOmitsHeaderWithoutToken(t *testing.T) {
	for _, value := range []string{"", "   "} {
		request, err := http.NewRequest(http.MethodGet, "https://codeload.github.com/owner/repo/zip/main", nil)
		if err != nil {
			t.Fatal(err)
		}
		t.Setenv("CANVAS_GITHUB_TOKEN", value)
		setGitHubAuthorization(request)
		if got := request.Header.Get("Authorization"); got != "" {
			t.Fatalf("空白令牌不应发出认证头: %q", got)
		}
	}
}

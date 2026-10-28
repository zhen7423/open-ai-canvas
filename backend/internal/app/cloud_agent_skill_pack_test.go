package app

import (
	"strings"
	"testing"

	"yingce/backend/internal/model"

	"gorm.io/gorm"
)

// 套装（同一 GitHub 仓库分别装入的多个技能）在运行侧的两条行为：
// 勾选总控即自动附带同套已启用配套；「技能名/包内路径」解析到对应配套技能。

func createPackTestSkill(t *testing.T, s *Service, name string, body string) string {
	t.Helper()
	skill, err := s.CreateSkill("user", SkillMutationRequest{SkillName: name, Description: name + " 的适用范围", Instruction: "# " + name + "\n\n" + body, Tag: "others"})
	if err != nil {
		t.Fatal(err)
	}
	return skill.SkillID
}

func markPackSource(t *testing.T, db *gorm.DB, id string, sourceURL string, ownerID string) {
	t.Helper()
	updates := map[string]any{"source_type": "github", "source_url": sourceURL, "source_ref": "main"}
	if ownerID != "" {
		updates["owner_id"] = ownerID
	}
	if err := db.Model(&model.Skill{}).Where("id = ?", id).Updates(updates).Error; err != nil {
		t.Fatal(err)
	}
}

func snapshotNames(snapshots []cloudAgentSkill) []string {
	names := make([]string, 0, len(snapshots))
	for _, skill := range snapshots {
		names = append(names, skill.Name)
	}
	return names
}

func TestCloudAgentSkillsAutoAttachPackCompanions(t *testing.T) {
	s, db, _, _ := creationTestService(t)
	const packRepo = "https://github.com/acme/manju-suite"
	master := createPackTestSkill(t, s, "short-drama-director", "总控正文")
	subA := createPackTestSkill(t, s, "sd-script-engine", "剧本层正文")
	subB := createPackTestSkill(t, s, "sd-qc-engine", "质检层正文")
	solo := createPackTestSkill(t, s, "other-repo-skill", "别的工作流")
	for _, id := range []string{master, subA} {
		markPackSource(t, db, id, packRepo, "")
	}
	// solo 与总控同仓不同包：装在另一个仓库里，不该被附带。
	markPackSource(t, db, solo, "https://github.com/acme/another-suite", "")
	// subB 属于同一仓库，但当前用户既不是作者、也没启用（没有 added 状态）：不得被附带。
	// 注意 CreateSkill 会给创建者留一条 added=true 的状态行，所以必须把 owner 改走并删掉该行，
	// 否则它仍然是「该用户已启用」，附带它才是正确行为。
	markPackSource(t, db, subB, packRepo, "someone-else")
	if err := db.Where("skill_id = ?", subB).Delete(&model.UserSkillState{}).Error; err != nil {
		t.Fatal(err)
	}

	snapshots, err := s.cloudAgentSkills("user", []string{master})
	if err != nil {
		t.Fatal(err)
	}
	// 顺序：勾选的在前，附带的按名称排在后面。
	if got := strings.Join(snapshotNames(snapshots), ","); got != "short-drama-director,sd-script-engine" {
		t.Fatalf("attached snapshots = %q, want 勾选的总控 + 同套已启用配套", got)
	}
	attached := map[string]string{}
	for _, skill := range snapshots {
		attached[skill.Name] = skill.AttachedVia
	}
	if attached["short-drama-director"] != "" {
		t.Fatalf("勾选的技能不该标记为附带：%+v", snapshots)
	}
	if attached["sd-script-engine"] != cloudAgentAttachedViaPack {
		t.Fatalf("配套技能应标记 attachedVia=pack：%+v", snapshots)
	}

	// 附带的配套真的可读，并且读到的版本就是它自己的。
	state := cloudAgentRuntime{Skills: snapshots}
	result, err := cloudAgentReadTool(nil, "user", &state, skillFeedbackCall(master, "sd-script-engine/SKILL.md"), s)
	if err != nil {
		t.Fatal(err)
	}
	page, ok := result.(map[string]any)
	if !ok || !strings.Contains(page["content"].(string), "剧本层正文") {
		t.Fatalf("alias read returned the wrong skill body: %#v", result)
	}
	if page["version"] == nil || page["version"] != attachedVersion(snapshots, "sd-script-engine") {
		t.Fatalf("alias read must pin the companion version: %#v", page)
	}

	// 未启用的同套技能不因附带变得可读。
	if _, err := cloudAgentReadTool(nil, "user", &cloudAgentRuntime{Skills: snapshots}, skillFeedbackCall(subB, cloudAgentSkillEntryPath), s); err == nil {
		t.Fatal("未启用的同套技能被读到了")
	}
	// 别的仓库不进本轮。
	for _, skill := range snapshots {
		if skill.ID == solo {
			t.Fatal("别的仓库的技能被附带进本轮")
		}
	}
}

func TestCloudAgentSkillsWithoutPackStayUnchanged(t *testing.T) {
	s, _, _, _ := creationTestService(t)
	skillID := createPackTestSkill(t, s, "single-skill", "只有这一个技能")
	snapshots, err := s.cloudAgentSkills("user", []string{skillID})
	if err != nil {
		t.Fatal(err)
	}
	if len(snapshots) != 1 || snapshots[0].AttachedVia != "" {
		t.Fatalf("非套装技能被改写：%+v", snapshots)
	}
	// 未安装且启用的技能仍然直接报错，不因套装逻辑变成静默跳过；边界文案一并钉住。
	_, err = s.cloudAgentSkills("other-user", []string{skillID})
	if err == nil {
		t.Fatal("非归属用户仍可读别人的技能")
	}
	if !strings.Contains(err.Error(), "只能使用用户技能库中已安装且启用的技能") {
		t.Fatalf("鉴权边界文案变了：%v", err)
	}
}

func attachedVersion(snapshots []cloudAgentSkill, name string) string {
	for _, skill := range snapshots {
		if skill.Name == name {
			return skill.Version
		}
	}
	return ""
}

func TestCloudAgentSkillPathResolvesPackSibling(t *testing.T) {
	const packA = "https://github.com/acme/manju-suite"
	packASkills := []cloudAgentSkill{
		{ID: "master", Name: "short-drama-director", Version: "v1", Pack: packA, Instruction: "# 总控", Files: map[string]string{cloudAgentSkillEntryPath: "", "references/track-routing.md": ""}},
		{ID: "sub", Name: "sd-script-engine", Version: "v2", Pack: packA, Instruction: "# 剧本", Files: map[string]string{cloudAgentSkillEntryPath: "", "references/six-stage-pipeline.md": "台词拟人化引擎"}},
	}
	for _, testCase := range []struct {
		name        string
		skillID     string
		path        string
		wantSkill   string
		wantPath    string
		wantContent string
	}{
		{name: "跨目录读配套的卡", skillID: "master", path: "sd-script-engine/references/six-stage-pipeline.md", wantSkill: "sub", wantPath: "references/six-stage-pipeline.md", wantContent: "台词拟人化引擎"},
		{name: "跨目录读配套的入口", skillID: "master", path: "sd-script-engine/SKILL.md", wantSkill: "sub", wantPath: cloudAgentSkillEntryPath},
		{name: "技能名大小写与空格", skillID: "master", path: " SD-Script-Engine /references/six-stage-pipeline.md ", wantSkill: "sub", wantPath: "references/six-stage-pipeline.md", wantContent: "台词拟人化引擎"},
		{name: "自家技能带自己的名字前缀", skillID: "sub", path: "short-drama-director/references/track-routing.md", wantSkill: "master", wantPath: "references/track-routing.md"},
		{name: "普通包内路径不改写", skillID: "sub", path: "references/six-stage-pipeline.md", wantSkill: "sub", wantPath: "references/six-stage-pipeline.md", wantContent: "台词拟人化引擎"},
		{name: "首段像技能名但路径不存在", skillID: "master", path: "sd-script-engine/references/missing.md", wantSkill: "master", wantPath: "sd-script-engine/references/missing.md"},
		{name: "只写技能名不改写", skillID: "master", path: "sd-script-engine", wantSkill: "master", wantPath: "sd-script-engine"},
	} {
		skillID, filePath := cloudAgentResolveSkillPath(packASkills, testCase.skillID, testCase.path)
		if skillID != testCase.wantSkill || filePath != testCase.wantPath {
			t.Errorf("%s: resolve(%q) = (%q, %q), want (%q, %q)", testCase.name, testCase.path, skillID, filePath, testCase.wantSkill, testCase.wantPath)
		}
	}

	// 别名只负责寻址：解析到配套后，读取仍按该配套自己的快照内容走。
	state := cloudAgentRuntime{Skills: packASkills}
	result, err := cloudAgentReadTool(nil, "user", &state, skillFeedbackCall("master", "sd-script-engine/references/six-stage-pipeline.md"))
	if err != nil {
		t.Fatal(err)
	}
	data := result.(map[string]any)
	if data["version"] != "v2" || data["path"] != "references/six-stage-pipeline.md" || data["content"] != "台词拟人化引擎" {
		t.Fatalf("alias read did not resolve to the companion snapshot: %#v", data)
	}
	if _, err := cloudAgentReadTool(nil, "user", &cloudAgentRuntime{Skills: packASkills}, skillFeedbackCall("master", "sd-script-engine/references/missing.md")); err == nil {
		t.Fatal("别名解析到不存在的参考文件却成功了")
	}
}

// 同名但不同套装的技能不能劫持寻址；同套装里出现两只同名时按歧义处理、宁可不改写。
func TestCloudAgentSkillPathAliasStaysInsideItsOwnPack(t *testing.T) {
	const packA = "https://github.com/acme/manju-suite"
	const packB = "https://github.com/other/fork-of-suite"
	skills := []cloudAgentSkill{
		{ID: "master", Name: "short-drama-director", Pack: packA, Files: map[string]string{cloudAgentSkillEntryPath: ""}},
		{ID: "sub", Name: "sd-script-engine", Pack: packA, Files: map[string]string{"references/six-stage-pipeline.md": "正版"}},
		{ID: "impostor", Name: "sd-script-engine", Pack: packB, Files: map[string]string{"references/six-stage-pipeline.md": "冒牌"}},
	}
	skillID, filePath := cloudAgentResolveSkillPath(skills, "master", "sd-script-engine/references/six-stage-pipeline.md")
	if skillID != "sub" || filePath != "references/six-stage-pipeline.md" {
		t.Fatalf("同名的另一套技能劫持了寻址：(%q, %q)", skillID, filePath)
	}

	ambiguous := []cloudAgentSkill{
		{ID: "master", Name: "short-drama-director", Pack: packA, Files: map[string]string{cloudAgentSkillEntryPath: ""}},
		{ID: "sub", Name: "sd-script-engine", Pack: packA, Files: map[string]string{"references/six-stage-pipeline.md": "第一只"}},
		{ID: "twin", Name: "sd-script-engine", Pack: packA, Files: map[string]string{"references/six-stage-pipeline.md": "第二只"}},
	}
	if gotID, gotPath := cloudAgentResolveSkillPath(ambiguous, "master", "sd-script-engine/references/six-stage-pipeline.md"); gotID != "master" || gotPath != "sd-script-engine/references/six-stage-pipeline.md" {
		t.Fatalf("同名歧义时不该改写，实际改成 (%q, %q)", gotID, gotPath)
	}

	// 没有套装标识的技能之间也不互相改写（别名只服务套装场景）。
	loose := []cloudAgentSkill{
		{ID: "plain", Name: "plain-skill", Files: map[string]string{"references/a.md": "自己"}},
		{ID: "other", Name: "other-skill", Files: map[string]string{"references/a.md": "别人"}},
	}
	if gotID, gotPath := cloudAgentResolveSkillPath(loose, "plain", "other-skill/references/a.md"); gotID != "plain" || gotPath != "other-skill/references/a.md" {
		t.Fatalf("非套装技能之间发生了改写：(%q, %q)", gotID, gotPath)
	}
}

// 使用统计按真正被读的那只技能归账：别名落到配套上时，事件里的 skillId/skillName/path
// 都得是配套的，原始请求留在 requestedSkillId/requestedPath。
func TestCloudAgentSkillReadEventAttributesAliasToCompanion(t *testing.T) {
	const packA = "https://github.com/acme/manju-suite"
	skills := []cloudAgentSkill{
		{ID: "master", Name: "short-drama-director", Pack: packA, Files: map[string]string{cloudAgentSkillEntryPath: ""}},
		{ID: "sub", Name: "sd-script-engine", Pack: packA, Files: map[string]string{"references/six-stage-pipeline.md": "台词拟人化引擎"}},
	}
	state := cloudAgentRuntime{Skills: skills}
	call := skillFeedbackCall("master", "sd-script-engine/references/six-stage-pipeline.md")
	cloudAgentToolResult("run-1", &state, call, map[string]any{"content": "台词拟人化引擎"}, nil)
	if len(state.Events) != 1 {
		t.Fatalf("事件数 = %d", len(state.Events))
	}
	payload := state.Events[0].Payload
	if payload["skillId"] != "sub" || payload["skillName"] != "sd-script-engine" || payload["path"] != "references/six-stage-pipeline.md" {
		t.Fatalf("别名读取没有归账到配套：%+v", payload)
	}
	if payload["requestedSkillId"] != "master" || payload["requestedPath"] != "sd-script-engine/references/six-stage-pipeline.md" {
		t.Fatalf("原始请求没被保留：%+v", payload)
	}
}

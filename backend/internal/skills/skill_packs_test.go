package skills

import (
	"fmt"
	"sort"
	"strings"
	"testing"

	"yingce/backend/internal/kernel"
	"yingce/backend/internal/model"
	"yingce/backend/internal/repository"

	"gorm.io/driver/sqlite"
	"gorm.io/gorm"
	"gorm.io/gorm/logger"
)

// 套装（同一 GitHub 仓库分别装入的多个技能）的两条行为：
// 启用任意一个 → 同套公开已上架配套连带启用；移除 → 不级联。

const packTestRepo = "https://github.com/acme/manju-suite"

func newSkillPackTestService(t *testing.T) (*Service, *gorm.DB) {
	t.Helper()
	db, err := gorm.Open(sqlite.Open("file:"+kernel.NewID()+"?mode=memory&cache=shared"), &gorm.Config{Logger: logger.Default.LogMode(logger.Silent)})
	if err != nil {
		t.Fatal(err)
	}
	sqlDB, err := db.DB()
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = sqlDB.Close() })
	if err := db.AutoMigrate(&model.SkillLibraryCategory{}, &model.UserSkillState{}, &model.Skill{}, &model.User{}, &model.UserIdentity{}); err != nil {
		t.Fatal(err)
	}
	return New(repository.New(db), t.TempDir(), nil), db
}

func packFixtureSkill(id string, name string, sourceType string, sourceURL string, status int, isPrivate bool, ownerID string) model.Skill {
	return model.Skill{
		ID: id, OwnerID: ownerID, Name: name, Description: "配套", Instruction: "# " + name + "\n\n正文。",
		CurrentVersionID: "", VersionLabel: "1.0.0", SourceType: sourceType, SourceURL: sourceURL,
		SourceRef: "main", Status: status, Source: skillSourceUser, IsPrivate: isPrivate, ShowcaseMediaJSON: "[]",
	}
}

func packAddedSkillIDs(t *testing.T, db *gorm.DB, userID string) []string {
	t.Helper()
	var states []model.UserSkillState
	if err := db.Where("user_id = ? AND added = ?", userID, true).Order("skill_id asc").Find(&states).Error; err != nil {
		t.Fatal(err)
	}
	ids := make([]string, 0, len(states))
	for _, state := range states {
		ids = append(ids, state.SkillID)
	}
	// 排序在 Go 里做，不依赖数据库的 ORDER BY 对照规则（sqlite 是 BINARY，生产可能不是）。
	sort.Strings(ids)
	return ids
}

func seedPackSkills(t *testing.T, db *gorm.DB) {
	t.Helper()
	for _, skill := range []model.Skill{
		packFixtureSkill("master", "总控", "github", packTestRepo, skillStatusEnabled, false, "author"),
		packFixtureSkill("w-script", "sd-script-engine", "github", packTestRepo, skillStatusEnabled, false, "author"),
		packFixtureSkill("w-qc", "sd-qc-engine", "github", packTestRepo, skillStatusEnabled, false, "author"),
		// 以下四条都不该被级联带出：已下架、私有、别的仓库、非 GitHub 来源。
		packFixtureSkill("off-shelf", "sd-off-shelf", "github", packTestRepo, 0, false, "author"),
		packFixtureSkill("private", "sd-private", "github", packTestRepo, skillStatusEnabled, true, "author"),
		packFixtureSkill("other-repo", "sd-other-repo", "github", "https://github.com/acme/another-suite", skillStatusEnabled, false, "author"),
		packFixtureSkill("manual", "sd-manual-upload", "markdown", packTestRepo, skillStatusEnabled, false, "author"),
	} {
		if err := db.Create(&skill).Error; err != nil {
			t.Fatal(err)
		}
	}
	if err := db.Create(&model.User{ID: "author", Username: "author", Role: model.UserRoleUser}).Error; err != nil {
		t.Fatal(err)
	}
}

func TestSetSkillAddedCascadesPackCompanionsOnly(t *testing.T) {
	svc, db := newSkillPackTestService(t)
	seedPackSkills(t, db)
	user := &model.User{ID: "user-1", Username: "pack-user", Role: model.UserRoleUser}
	if err := db.Create(user).Error; err != nil {
		t.Fatal(err)
	}

	item, err := svc.SetSkillAdded(user.ID, "master", true)
	if err != nil {
		t.Fatal(err)
	}
	if item.PackKey != skillPackKeyPrefix+packTestRepo {
		t.Fatalf("packKey = %q, want %q", item.PackKey, skillPackKeyPrefix+packTestRepo)
	}
	got := strings.Join(packCompanionNames(item.PackAutoAdded), ",")
	// 按名称排序：sd-qc-engine 在 sd-script-engine 之前；下架、私有、他仓、非 GitHub 来源都不进结果。
	if want := "sd-qc-engine,sd-script-engine"; got != want {
		t.Fatalf("PackAutoAdded = %q, want %q", got, want)
	}
	if ids := strings.Join(packAddedSkillIDs(t, db, user.ID), ","); ids != "master,w-qc,w-script" {
		t.Fatalf("added states = %q, want master,w-qc,w-script", ids)
	}

	// 再启用一次（幂等）：已经在库里的配套不能被重复计成「连带启用」。
	again, err := svc.SetSkillAdded(user.ID, "master", true)
	if err != nil {
		t.Fatal(err)
	}
	if len(again.PackAutoAdded) != 0 {
		t.Fatalf("repeat add cascaded again: %s", packCompanionNames(again.PackAutoAdded))
	}

	// 移除只作用于勾选的那一个，配套留在技能库里。
	if _, err := svc.SetSkillAdded(user.ID, "master", false); err != nil {
		t.Fatal(err)
	}
	if ids := strings.Join(packAddedSkillIDs(t, db, user.ID), ","); ids != "w-qc,w-script" {
		t.Fatalf("after removing master, added states = %q, want w-qc,w-script", ids)
	}
}

// 作者本人的技能恒在库中（不写状态），但级联照跑：同套、别人安装的配套仍需连带启用。
func TestSetSkillAddedCascadesForOwnedMaster(t *testing.T) {
	svc, db := newSkillPackTestService(t)
	user := &model.User{ID: "author-1", Username: "pack-author", Role: model.UserRoleUser}
	if err := db.Create(user).Error; err != nil {
		t.Fatal(err)
	}
	for _, skill := range []model.Skill{
		packFixtureSkill("own-master", "own-总控", "github", packTestRepo, skillStatusEnabled, false, user.ID),
		packFixtureSkill("own-sub", "sd-own-sub", "github", packTestRepo, skillStatusEnabled, false, "someone-else"),
	} {
		if err := db.Create(&skill).Error; err != nil {
			t.Fatal(err)
		}
	}

	item, err := svc.SetSkillAdded(user.ID, "own-master", true)
	if err != nil {
		t.Fatal(err)
	}
	if names := strings.Join(packCompanionNames(item.PackAutoAdded), ","); names != "sd-own-sub" {
		t.Fatalf("PackAutoAdded = %q, want sd-own-sub", names)
	}
	if ids := strings.Join(packAddedSkillIDs(t, db, user.ID), ","); ids != "own-sub" {
		t.Fatalf("added states = %q, want own-sub (自有技能不写状态)", ids)
	}
	if _, err := svc.SetSkillAdded(user.ID, "own-master", false); appErrorStatus(err) != 400 {
		t.Fatalf("removing an owned skill error = %v, want 400", err)
	}
}

// 没有套装标识的技能（非 GitHub 来源、缺仓库地址）保持旧行为：只动自己那一条。
func TestSetSkillAddedWithoutPackKeepsLegacyBehaviour(t *testing.T) {
	svc, db := newSkillPackTestService(t)
	user := &model.User{ID: "user-1", Username: "pack-user", Role: model.UserRoleUser}
	if err := db.Create(user).Error; err != nil {
		t.Fatal(err)
	}
	for _, skill := range []model.Skill{
		packFixtureSkill("plain", "普通技能", "markdown", "", skillStatusEnabled, false, "author"),
		packFixtureSkill("github-no-url", "缺来源地址", "github", "", skillStatusEnabled, false, "author"),
	} {
		if err := db.Create(&skill).Error; err != nil {
			t.Fatal(err)
		}
	}
	for _, id := range []string{"plain", "github-no-url"} {
		item, err := svc.SetSkillAdded(user.ID, id, true)
		if err != nil {
			t.Fatal(err)
		}
		if item.PackKey != "" || len(item.PackAutoAdded) != 0 {
			t.Fatalf("skill %q must not join a pack: key=%q autoAdded=%v", id, item.PackKey, item.PackAutoAdded)
		}
	}
	if ids := strings.Join(packAddedSkillIDs(t, db, user.ID), ","); ids != "github-no-url,plain" {
		t.Fatalf("added states = %q", ids)
	}
}

func TestAddedPackCompanionsRequiresUserLibrary(t *testing.T) {
	svc, db := newSkillPackTestService(t)
	seedPackSkills(t, db)
	user := &model.User{ID: "user-1", Username: "pack-user", Role: model.UserRoleUser}
	if err := db.Create(user).Error; err != nil {
		t.Fatal(err)
	}
	for _, state := range []model.UserSkillState{
		{ID: "state-master", UserID: user.ID, SkillID: "master", Added: true},
		{ID: "state-script", UserID: user.ID, SkillID: "w-script", Added: true},
	} {
		if err := db.Create(&state).Error; err != nil {
			t.Fatal(err)
		}
	}

	companions, err := svc.AddedPackCompanions(user.ID, "master")
	if err != nil {
		t.Fatal(err)
	}
	if names := strings.Join(packCompanionNames(companions), ","); names != "sd-script-engine" {
		t.Fatalf("companions = %q, want only the one already in this user's library (w-qc 未启用)", names)
	}
	// 自我附带要挡掉：按 id 点名被查询的技能，不看返回条数（条数断言会让这条永不触发）。
	for _, companion := range companions {
		if companion.SkillID == "master" {
			t.Fatalf("the requested skill attached to itself: %v", packCompanionIDs(companions))
		}
	}
	// 非套装技能没有配套可附带。
	if others, err := svc.AddedPackCompanions(user.ID, "other-repo"); err != nil {
		t.Fatal(err)
	} else if len(others) != 0 {
		t.Fatalf("single-skill repo must not fabricate companions: %v", others)
	}
}

// packCompanionNames 返回排序后的配套名称：断言只关心集合成员，
// 不把关乎数据库排序规则的顺序当成被测行为。
func packCompanionNames(companions []SkillPackCompanion) []string {
	names := make([]string, 0, len(companions))
	for _, companion := range companions {
		names = append(names, companion.SkillName)
	}
	sort.Strings(names)
	return names
}

func packCompanionIDs(companions []SkillPackCompanion) []string {
	ids := make([]string, 0, len(companions))
	for _, companion := range companions {
		ids = append(ids, companion.SkillID)
	}
	sort.Strings(ids)
	return ids
}

// 级联上限：一个仓库里装了远超「一套」的数量时，只带出 skillPackCompanionLimit 条配套，
// 排在末尾的留在库里不动。常数被改错时这条会红——附带集合悄悄变大是最难发现的回归。
func TestSetSkillAddedCascadeStopsAtCompanionLimit(t *testing.T) {
	svc, db := newSkillPackTestService(t)
	user := &model.User{ID: "user-1", Username: "pack-user", Role: model.UserRoleUser}
	if err := db.Create(user).Error; err != nil {
		t.Fatal(err)
	}
	master := packFixtureSkill("cap-master", "a-master", "github", packTestRepo, skillStatusEnabled, false, "author")
	if err := db.Create(&master).Error; err != nil {
		t.Fatal(err)
	}
	total := skillPackCompanionLimit + 2
	for index := 0; index < total; index++ {
		sibling := packFixtureSkill(fmt.Sprintf("cap-%02d", index), fmt.Sprintf("zz-sibling-%02d", index), "github", packTestRepo, skillStatusEnabled, false, "author")
		if err := db.Create(&sibling).Error; err != nil {
			t.Fatal(err)
		}
	}

	item, err := svc.SetSkillAdded(user.ID, "cap-master", true)
	if err != nil {
		t.Fatal(err)
	}
	if len(item.PackAutoAdded) != skillPackCompanionLimit {
		t.Fatalf("cascaded companions = %d, want the limit %d", len(item.PackAutoAdded), skillPackCompanionLimit)
	}
	if added := packAddedSkillIDs(t, db, user.ID); len(added) != skillPackCompanionLimit+1 {
		t.Fatalf("added states = %d, want %d (被点的一个 + 上限数量的配套)", len(added), skillPackCompanionLimit+1)
	}
	overflowID := fmt.Sprintf("cap-%02d", total-1)
	var overflow int64
	if err := db.Model(&model.UserSkillState{}).Where("user_id = ? AND skill_id = ? AND added = ?", user.ID, overflowID, true).Count(&overflow).Error; err != nil {
		t.Fatal(err)
	}
	if overflow != 0 {
		t.Fatalf("超出上限的配套 %s 被连带启用了", overflowID)
	}
}

// 级联启用不能抹掉用户先前给配套留下的东西：「已移出技能库但保留自定义库分类/收藏」
// 是平台允许存在的状态，而加入用的 upsert 会覆盖 library_category_id ——
// 所以配套必须复用已有行（连 ID 一起），不能拿一条全新空行去盖。
func TestSetSkillAddedCascadePreservesExistingCompanionState(t *testing.T) {
	svc, db := newSkillPackTestService(t)
	user := &model.User{ID: "user-1", Username: "pack-user", Role: model.UserRoleUser}
	if err := db.Create(user).Error; err != nil {
		t.Fatal(err)
	}
	for _, skill := range []model.Skill{
		packFixtureSkill("cat-master", "cat-总控", "github", packTestRepo, skillStatusEnabled, false, "author"),
		packFixtureSkill("cat-sub", "sd-cat-sub", "github", packTestRepo, skillStatusEnabled, false, "author"),
	} {
		if err := db.Create(&skill).Error; err != nil {
			t.Fatal(err)
		}
	}
	previous := model.UserSkillState{ID: "state-sub", UserID: user.ID, SkillID: "cat-sub", Added: false, Liked: true, LibraryCategoryID: "writer-room"}
	if err := db.Create(&previous).Error; err != nil {
		t.Fatal(err)
	}

	if _, err := svc.SetSkillAdded(user.ID, "cat-master", true); err != nil {
		t.Fatal(err)
	}
	var state model.UserSkillState
	if err := db.First(&state, "user_id = ? AND skill_id = ?", user.ID, "cat-sub").Error; err != nil {
		t.Fatal(err)
	}
	if !state.Added {
		t.Fatal("配套没被连带启用")
	}
	if state.LibraryCategoryID != "writer-room" {
		t.Fatalf("级联清掉了用户的库分类：%q", state.LibraryCategoryID)
	}
	if !state.Liked {
		t.Fatal("级联把收藏状态冲掉了")
	}
	if state.ID != "state-sub" {
		t.Fatalf("级联换掉了原有状态行的 ID：%q", state.ID)
	}
	var rows int64
	if err := db.Model(&model.UserSkillState{}).Where("user_id = ? AND skill_id = ?", user.ID, "cat-sub").Count(&rows).Error; err != nil {
		t.Fatal(err)
	}
	if rows != 1 {
		t.Fatalf("同一 (user, skill) 出现了 %d 行状态", rows)
	}
}

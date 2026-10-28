// 套装（Skill Pack）：同一个 GitHub 仓库分别装入平台的多个技能目录视为一套
// （典型是「1 总控 + 12 子技能」这类多技能套装）。套装只做两件事：
//
//	① 启用其中任意一个时，连带把同套的公开已上架技能加入该用户的技能库；
//	② 本轮 Agent 挂了套装成员时，自动把该用户已启用的其余配套一起带进快照。
//
// 鉴权边界没有被放宽：进快照的技能仍然必须「该用户已加入 + 已上架 + 对本人可见」，
// 级联只写该用户自己的技能库状态，不改技能内容、上架状态或他人状态。
// 判定依据是安装时落库的来源仓库地址（SourceType=github + SourceURL），
// 因此不依赖上游作者在 SKILL.md 里声明套装；非 GitHub 来源与缺地址的技能不参与套装。

package skills

import (
	"strings"

	"yingce/backend/internal/kernel"
	"yingce/backend/internal/model"
)

const (
	// skillPackCompanionLimit 是**一次级联启用**最多带出多少个配套（不含被点的那个）。
	// 超过这个数说明该仓库装的不是同一套技能，而是被不同作者分别装了不相干的目录——
	// 宁可不带。注意它管的是「配套条数」，运行侧那个 32（cloudAgentPackAttachMax）管的是
	// 「本轮快照的技能总数」，两者语义不同，不要当成同一个数改来改去。
	skillPackCompanionLimit = 32
	// 套装标识前缀：写进接口的 packKey。
	skillPackKeyPrefix = "github:"
)

// SkillPackCompanion 是套装内除自己以外的一个配套技能。
type SkillPackCompanion struct {
	SkillID   string `json:"skillId"`
	SkillName string `json:"skillName"`
}

// skillPackKey 返回技能的套装标识；不参与套装的技能返回空串。
//
// 标识用**原样存储的仓库地址**，不做大小写折叠：分组查询 `source_url = ?` 同样按原样匹配，
// 两者必须同一个口径。若在这里小写，就会出现「packKey 声称同一套、查询却带不出」的假承诺。
func skillPackKey(skill *model.Skill) string {
	sourceURL := packSourceURL(skill)
	if sourceURL == "" {
		return ""
	}
	return skillPackKeyPrefix + sourceURL
}

// packSourceURL 取出用于分组套装的仓库规范地址（GitHub 安装时写入的就是规范化的
// https://github.com/owner/repo，不带结尾斜杠）。
func packSourceURL(skill *model.Skill) string {
	if skill == nil {
		return ""
	}
	return packSourceURLFrom(skill.SourceType, skill.SourceURL)
}

// packSourceURLFrom 是套装分组的唯一口径：只有带仓库地址的 GitHub 来源技能才成套。
func packSourceURLFrom(sourceType string, sourceURL string) string {
	if sourceType != "github" {
		return ""
	}
	return strings.TrimRight(strings.TrimSpace(sourceURL), "/")
}

// PackSourceURL 返回该技能的套装来源仓库地址；不参与套装时返回空串。
// 运行侧（本轮快照的自动附带）与写入侧（级联启用）都走这一个判断，避免两处口径漂移。
func (i SkillItem) PackSourceURL() string {
	return packSourceURLFrom(i.SourceType, i.SourceURL)
}

// packStatesForAdd 计算「启用 skill 时应当连带启用」的配套技能状态。
// 已在该用户技能库里的配套不重复写入，也不计入返回结果；查询失败不静默吞掉，
// 因为调用方要把主技能与配套放在同一事务里写，半途失败必须整体回滚。
func (s *Service) packStatesForAdd(userID string, skill *model.Skill) ([]*model.UserSkillState, []SkillPackCompanion, error) {
	sourceURL := packSourceURL(skill)
	if sourceURL == "" {
		return nil, nil, nil
	}
	siblings, err := s.repo.PackPublicSkills(sourceURL, skill.ID, skillPackCompanionLimit)
	if err != nil {
		return nil, nil, err
	}
	candidates := make([]model.Skill, 0, len(siblings))
	ids := make([]string, 0, len(siblings))
	for _, sibling := range siblings {
		candidates = append(candidates, sibling)
		ids = append(ids, sibling.ID)
	}
	if len(candidates) == 0 {
		return nil, nil, nil
	}
	states, err := s.repo.UserSkillStatesBySkillIDs(userID, ids)
	if err != nil {
		return nil, nil, err
	}
	added := make(map[string]bool, len(states))
	existing := make(map[string]model.UserSkillState, len(states))
	for _, state := range states {
		if state.Added {
			added[state.SkillID] = true
		}
		existing[state.SkillID] = state
	}
	batch := make([]*model.UserSkillState, 0, len(candidates))
	companions := make([]SkillPackCompanion, 0, len(candidates))
	for _, sibling := range candidates {
		if added[sibling.ID] {
			continue
		}
		// 复用已有行的 ID 与 library_category_id / liked：upsert 的 DO UPDATE 里带着
		// library_category_id，拿一条全新空行去覆盖，会把用户先前给这只技能选过的库分类抹掉
		// （「已移出但保留自定义分类」是平台允许存在的形态）。
		state := model.UserSkillState{ID: kernel.NewID(), UserID: userID, SkillID: sibling.ID}
		if previous, found := existing[sibling.ID]; found {
			state.ID = previous.ID
			state.LibraryCategoryID = previous.LibraryCategoryID
			state.Liked = previous.Liked
		}
		state.InstalledVersionID = sibling.CurrentVersionID
		state.AutoUpdate = sibling.AutoUpdate
		state.Added = true
		batch = append(batch, &state)
		companions = append(companions, SkillPackCompanion{SkillID: sibling.ID, SkillName: sibling.Name})
	}
	return batch, companions, nil
}

// AddedPackCompanions 返回该用户技能库里、与指定技能同套装的其余配套（不含自己，按名称排序）。
// 本轮快照的自动附带只在这个集合里取，因此进来的技能本来就满足「已启用」这条边界。
func (s *Service) AddedPackCompanions(userID string, skillID string) ([]SkillPackCompanion, error) {
	skill, err := s.visibleSkill(userID, skillID)
	if err != nil {
		return nil, err
	}
	sourceURL := packSourceURL(skill)
	if sourceURL == "" {
		return nil, nil
	}
	skills, err := s.repo.PackAddedSkills(userID, sourceURL, skill.ID, skillPackCompanionLimit)
	if err != nil {
		return nil, err
	}
	companions := make([]SkillPackCompanion, 0, len(skills))
	for _, sibling := range skills {
		companions = append(companions, SkillPackCompanion{SkillID: sibling.ID, SkillName: sibling.Name})
	}
	return companions, nil
}

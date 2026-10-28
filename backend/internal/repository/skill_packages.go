package repository

import (
	"strings"

	"yingce/backend/internal/model"

	"gorm.io/gorm"
)

func (r *Repository) CreateSkillWithPackage(skill *model.Skill, version *model.SkillVersion, files []model.SkillFile, ownerState *model.UserSkillState) error {
	return r.db.Transaction(func(tx *gorm.DB) error {
		if err := tx.Create(skill).Error; err != nil {
			return err
		}
		if err := tx.Create(version).Error; err != nil {
			return err
		}
		if len(files) > 0 {
			if err := tx.Create(&files).Error; err != nil {
				return err
			}
		}
		return tx.Create(ownerState).Error
	})
}

func (r *Repository) AddSkillVersion(skill *model.Skill, version *model.SkillVersion, files []model.SkillFile) error {
	return r.db.Transaction(func(tx *gorm.DB) error {
		if err := tx.Create(version).Error; err != nil {
			return err
		}
		if len(files) > 0 {
			if err := tx.Create(&files).Error; err != nil {
				return err
			}
		}
		return tx.Save(skill).Error
	})
}

// AddSkillVersionIfCurrent 以 current_version_id 做乐观锁：只有技能当前版本仍是 baseVersionID 时
// 才写入新版本、文件清单和技能行；返回 false 表示已被并发修改，事务内不做任何写入。
func (r *Repository) AddSkillVersionIfCurrent(skill *model.Skill, version *model.SkillVersion, files []model.SkillFile, baseVersionID string) (bool, error) {
	applied := false
	err := r.db.Transaction(func(tx *gorm.DB) error {
		result := tx.Model(&model.Skill{}).Where("id = ? AND current_version_id = ?", skill.ID, baseVersionID).Update("current_version_id", version.ID)
		if result.Error != nil {
			return result.Error
		}
		if result.RowsAffected == 0 {
			return nil
		}
		if err := tx.Create(version).Error; err != nil {
			return err
		}
		if len(files) > 0 {
			if err := tx.Create(&files).Error; err != nil {
				return err
			}
		}
		if err := tx.Save(skill).Error; err != nil {
			return err
		}
		applied = true
		return nil
	})
	return applied, err
}

func (r *Repository) SkillVersion(id string) (*model.SkillVersion, error) {
	var version model.SkillVersion
	if err := r.db.First(&version, "id = ?", id).Error; err != nil {
		return nil, err
	}
	return &version, nil
}

func (r *Repository) SkillFiles(versionID string) ([]model.SkillFile, error) {
	var files []model.SkillFile
	err := r.db.Where("skill_version_id = ?", versionID).Order("path asc").Find(&files).Error
	return files, err
}

func (r *Repository) SkillsForPackageEnsure() ([]model.Skill, error) {
	var skills []model.Skill
	err := r.db.Where("status = ?", 1).Find(&skills).Error
	return skills, err
}

func (r *Repository) AutoUpdatingGitHubSkills() ([]model.Skill, error) {
	var skills []model.Skill
	err := r.db.Where("status = ? AND source_type = ? AND auto_update = ?", 1, "github", true).Find(&skills).Error
	return skills, err
}

func (r *Repository) SaveSkill(skill *model.Skill) error {
	return r.db.Save(skill).Error
}

// PackPublicSkills 返回同一 GitHub 仓库来源、且已公开上架的套装技能（按名称稳定排序）。
// excludeID 在 SQL 里就排掉被点的那个技能，这样 limit 数的就是「配套条数」本身，
// 不会因为被点的技能按名称排在页里而少带一条。
// 私有技能不进结果：本人自有的配套技能在平台口径里本来就已启用（IsAdded 对 owner 恒真），
// 不需要级联写状态；别人的私有技能本就不该被带进任何人的技能库。
func (r *Repository) PackPublicSkills(sourceURL string, excludeID string, limit int) ([]model.Skill, error) {
	if strings.TrimSpace(sourceURL) == "" || limit <= 0 {
		return nil, nil
	}
	query := r.db.Where("status = ? AND is_private = ? AND source_type = ? AND source_url = ?", 1, false, "github", sourceURL)
	if excludeID = strings.TrimSpace(excludeID); excludeID != "" {
		query = query.Where("id <> ?", excludeID)
	}
	var skills []model.Skill
	err := query.Order("name asc").Limit(limit).Find(&skills).Error
	return skills, err
}

// PackAddedSkills 返回该用户技能库里已启用（已加入，或本人创建，含本人私有）的同套装技能。
// 本轮快照的「自动附带」只从这个集合里取，鉴权边界与逐个勾选时完全一致；
// excludeID 同样在 SQL 里排掉被查询的技能自己。
func (r *Repository) PackAddedSkills(userID string, sourceURL string, excludeID string, limit int) ([]model.Skill, error) {
	if strings.TrimSpace(userID) == "" || strings.TrimSpace(sourceURL) == "" || limit <= 0 {
		return nil, nil
	}
	query := r.db.Model(&model.Skill{}).
		Select("skills.*").
		Joins("LEFT JOIN user_skill_states ON user_skill_states.skill_id = skills.id AND user_skill_states.user_id = ?", userID).
		Where("skills.status = ? AND skills.source_type = ? AND skills.source_url = ?", 1, "github", sourceURL).
		Where("skills.is_private = ? OR skills.owner_id = ?", false, userID).
		Where("skills.owner_id = ? OR user_skill_states.added = ?", userID, true)
	if excludeID = strings.TrimSpace(excludeID); excludeID != "" {
		query = query.Where("skills.id <> ?", excludeID)
	}
	var skills []model.Skill
	err := query.Order("skills.name asc").Limit(limit).Find(&skills).Error
	return skills, err
}

import { useEffect, useMemo, useState } from "react";


import { listAddedSkills, type Skill } from "@/services/api/skills";
import { SKILL_RUNTIME_PROFILES, type SkillRuntimeProfile } from "@/services/skill-runtime";
import { Select } from "@/components/ui/base/select";

export function useSkillRuntimeCatalog() {
    const [skills, setSkills] = useState<Skill[]>([]);
    const [loading, setLoading] = useState(true);
    // 读失败和技能库为空要分开：读失败时不能据此清空已选。
    const [ready, setReady] = useState(false);

    useEffect(() => {
        let cancelled = false;
        listAddedSkills()
            .then((result) => {
                if (cancelled) return;
                setSkills(result.skills.filter((skill) => skill.isAdded));
                setReady(true);
            })
            .catch(() => {
                if (cancelled) return;
                setSkills([]);
                setReady(false);
            })
            .finally(() => {
                if (!cancelled) setLoading(false);
            });
        return () => {
            cancelled = true;
        };
    }, []);

    return { skills, loading, ready };
}

export function SkillRuntimePicker({ skills, loading, value, onChange, placeholder = "选择本次生成使用的技能", profile = "canvas" }: { skills: Skill[]; loading?: boolean; value: string[]; onChange: (skillIds: string[]) => void; placeholder?: string; profile?: SkillRuntimeProfile }) {
    const options = useMemo(
        () => skills.map((skill) => ({ value: skill.skillId, label: skill.skillName, title: skill.description })),
        [skills],
    );
    const maxSkills = SKILL_RUNTIME_PROFILES[profile].maxSkills;

    return (
        <Select
            className="w-full"
            style={{ width: "100%" }}
            mode="multiple"
            allowClear
            showSearch
            maxCount={maxSkills || undefined}
            maxTagCount="responsive"
            loading={loading}
            value={value}
            placeholder={placeholder}
            optionFilterProp="label"
            options={options}
            onChange={onChange}
            notFoundContent={loading ? "正在读取技能库" : "暂无已加入的技能"}
            aria-label="本次生成使用的技能"
        />
    );
}

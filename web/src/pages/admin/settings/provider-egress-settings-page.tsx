import { App, Button, Form, Input, Skeleton } from "antd";
import { Select, Switch } from "@/pages/admin/ui/controls";
import { AlertTriangle, CloudUpload, Eraser, KeyRound, RefreshCw, RotateCcw, Save, Zap } from "lucide-react";
import { useCallback, useEffect, useRef, useState } from "react";
import { useBlocker } from "react-router";

import { cn } from "@/lib/utils";
import { clearAdminProviderEgressMappings, getAdminProviderEgressSetting, testAdminProviderEgressConnection, updateAdminProviderEgressSetting, type AdminProviderEgressSetting } from "@/services/api/auth";
import { AdminPageFrame } from "../components/admin-shell";
import { AdminStatusBadge, configuredSecretText, SettingsSectionCard } from "../components/admin-ui";

type ProviderEgressForm = {
    enabled: boolean;
    provider: string;
    region: string;
    endpoint: string;
    bucket: string;
    accessKeyId: string;
    accessKeySecret: string;
    pathPrefix: string;
    cdnBaseUrl: string;
    cdnAuthMode: string;
};

type ProviderEgressPayload = Pick<AdminProviderEgressSetting, "enabled" | "provider" | "region" | "endpoint" | "bucket" | "accessKeyId" | "accessKeySecret" | "pathPrefix" | "cdnBaseUrl" | "cdnAuthMode">;

const providerOptions = [
    { value: "aliyun", label: "阿里云 OSS" },
    { value: "tencent", label: "腾讯云 COS" },
    { value: "qiniu", label: "七牛云 Kodo" },
    { value: "s3", label: "通用 S3" },
];

const cdnModeOptions = [
    { value: "", label: "不使用 CDN（回源签名地址）" },
    { value: "public", label: "CDN 公有读（地址不带签名参数）" },
    { value: "qiniu", label: "CDN 七牛私有鉴权" },
];

export default function ProviderEgressSettingsPage() {
    const { message, modal } = App.useApp();
    const [setting, setSetting] = useState<AdminProviderEgressSetting | null>(null);
    const [loading, setLoading] = useState(true);
    const [refreshing, setRefreshing] = useState(false);
    const [saving, setSaving] = useState(false);
    const [testing, setTesting] = useState(false);
    const [dirty, setDirty] = useState(false);
    const [loadError, setLoadError] = useState("");
    const [saveError, setSaveError] = useState("");
    const [form] = Form.useForm<ProviderEgressForm>();
    const requestVersionRef = useRef(0);
    const navigationConfirmOpenRef = useRef(false);
    const watchedValues = Form.useWatch([], form) as Partial<ProviderEgressForm> | undefined;

    const load = useCallback(
        async (initial = false, announce = false) => {
            const requestVersion = ++requestVersionRef.current;
            if (initial) setLoading(true);
            else setRefreshing(true);
            setLoadError("");
            try {
                const result = await getAdminProviderEgressSetting();
                if (requestVersion !== requestVersionRef.current) return;
                if (!isProviderEgressSetting(result.setting)) throw new Error("服务端返回的出站内镜配置格式无效");
                setSetting(result.setting);
                setDirty(false);
                setSaveError("");
                if (announce) message.success("已重新读取出站内镜配置");
            } catch (error) {
                if (requestVersion !== requestVersionRef.current) return;
                const errorMessage = error instanceof Error ? error.message : "读出站内镜配置失败";
                setLoadError(errorMessage);
                if (!initial) message.error(errorMessage);
            } finally {
                if (requestVersion === requestVersionRef.current) {
                    setLoading(false);
                    setRefreshing(false);
                }
            }
        },
        [message],
    );

    useEffect(() => {
        void load(true);
        return () => {
            requestVersionRef.current += 1;
        };
    }, [load]);

    useEffect(() => {
        if (loading || !setting) return;
        form.setFieldsValue(toFormValues(setting));
    }, [form, loading, setting]);

    const blocker = useBlocker(dirty && !saving);

    useEffect(() => {
        const beforeUnload = (event: BeforeUnloadEvent) => {
            if (!dirty || saving) return;
            event.preventDefault();
        };
        window.addEventListener("beforeunload", beforeUnload);
        return () => window.removeEventListener("beforeunload", beforeUnload);
    }, [dirty, saving]);

    useEffect(() => {
        if (blocker.state !== "blocked" || navigationConfirmOpenRef.current) return;
        navigationConfirmOpenRef.current = true;
        modal.confirm({
            title: "放弃出站内镜调整？",
            content: "当前页面有尚未保存的内镜配置草稿，离开后这些内容会丢失。服务端正在使用的配置不会改变。",
            okText: "放弃并离开",
            cancelText: "继续编辑",
            okButtonProps: { danger: true },
            onOk: () => {
                navigationConfirmOpenRef.current = false;
                blocker.proceed();
            },
            onCancel: () => {
                navigationConfirmOpenRef.current = false;
                blocker.reset();
            },
        });
    }, [blocker, modal]);

    const resetDraft = () => {
        if (!setting || saving) return;
        form.setFieldsValue(toFormValues(setting));
        setDirty(false);
        setSaveError("");
        message.info("已撤销出站内镜的未保存调整");
    };

    const requestRefresh = () => {
        if (!dirty) {
            void load(false, true);
            return;
        }
        modal.confirm({
            title: "放弃调整并重新读取？",
            content: "重新读取会丢弃当前内镜草稿，并以服务端配置为准。",
            okText: "放弃并刷新",
            cancelText: "继续编辑",
            okButtonProps: { danger: true },
            onOk: () => load(false, true),
        });
    };

    const currentDraft = (): ProviderEgressPayload | null => {
        try {
            return normalizePayload(form.getFieldsValue(true));
        } catch {
            return null;
        }
    };

    const runTest = async () => {
        if (!setting) return;
        const draft = currentDraft();
        if (!draft) return;
        const validationError = validateDraft(draft, setting);
        if (validationError) {
            message.error(validationError);
            return;
        }
        setTesting(true);
        try {
            const result = await testAdminProviderEgressConnection(draft);
            message.success(`出站内镜可读写，桶 ${result.bucket} 校验通过`);
        } catch (error) {
            const errorMessage = error instanceof Error ? error.message : "出站内镜连通性测试失败";
            setSaveError(`连通性测试失败：${errorMessage}`);
            message.error(errorMessage);
        } finally {
            setTesting(false);
        }
    };

    const runClearMappings = () => {
        modal.confirm({
            title: "删除已镜像对象并清空复用映射？",
            content: "会逐个删除已镜像到桶里的对象并丢弃对应映射，下一张需要公网地址的参考图将重新上传。删除失败的对象会保留其映射并在结果里报出数量。",
            okText: "删除并清空",
            cancelText: "取消",
            okButtonProps: { danger: true },
            onOk: async () => {
                try {
                    const result = await clearAdminProviderEgressMappings();
                    message.success(
                        result.remaining > 0
                            ? `已删除 ${result.deleted} 个镜像对象并丢弃对应映射；台账仍有 ${result.remaining} 条未处理（属于其他落点、删除失败或超出单次上限），本功能不会用当前凭据跨落点删除`
                            : `已删除 ${result.deleted} 个镜像对象并丢弃对应映射`,
                    );
                    await load(false, false);
                } catch (error) {
                    const errorMessage = error instanceof Error ? error.message : "清空出站内镜映射失败";
                    message.error(errorMessage);
                }
            },
        });
    };

    const save = async (values: ProviderEgressForm) => {
        if (!setting) return;
        const expected = normalizePayload(values);
        const expectedHasSecret = nextSecretPresence(expected, setting);
        setSaving(true);
        setSaveError("");
        try {
            const result = await updateAdminProviderEgressSetting(expected);
            if (!isProviderEgressSetting(result.setting) || !responseMatches(result.setting, expected, expectedHasSecret)) {
                throw new Error("服务端返回的内镜配置与本次保存内容不一致，请重新读取后核对");
            }
            setSetting(result.setting);
            form.setFieldsValue(toFormValues(result.setting));
            setDirty(false);
            message.success("出站内镜配置已保存");
        } catch (error) {
            const errorMessage = error instanceof Error ? error.message : "保存出站内镜配置失败";
            setSaveError(`${errorMessage}。未自动重试，请重新读取当前配置后再决定是否保存。`);
            message.error(errorMessage);
            throw error;
        } finally {
            setSaving(false);
        }
    };

    const submitSave = async () => {
        if (!setting) return;
        let values: ProviderEgressForm;
        try {
            values = await form.validateFields();
        } catch {
            return;
        }
        const validationError = validateDraft(normalizePayload(values), setting);
        if (validationError) {
            message.error(validationError);
            return;
        }
        try {
            await save(values);
        } catch {
            // 保存错误已在 save 中就地提示。
        }
    };

    if (loading && !setting) {
        return (
            <AdminPageFrame title="出站内镜" description="预算内的参考图片直接内联给上游，超出可内联上限的才镜像成公网地址" scroll>
                <div className="admin-settings-stack admin-egress-settings" aria-label="正在读出出站内镜配置" role="status">
                    <Skeleton active paragraph={{ rows: 8 }} />
                </div>
            </AdminPageFrame>
        );
    }

    if (!setting) {
        return (
            <AdminPageFrame title="出站内镜" description="预算内的参考图片直接内联给上游，超出可内联上限的才镜像成公网地址" scroll>
                <div className="admin-settings-stack admin-egress-settings">
                    <div className="flex flex-wrap items-center gap-3 rounded-lg border border-destructive/30 p-4" role="alert">
                        <AlertTriangle className="size-5 shrink-0 text-destructive" aria-hidden="true" />
                        <div className="min-w-0 flex-1">
                            <h2 className="text-sm font-semibold">无法读出出站内镜配置</h2>
                            <p className="text-sm text-muted-foreground">{loadError || "当前没有可显示的配置，请稍后重试。"}</p>
                        </div>
                        <Button icon={<RefreshCw className="size-4" />} loading={refreshing} onClick={() => void load(false, true)}>
                            重新读取
                        </Button>
                    </div>
                </div>
            </AdminPageFrame>
        );
    }

    const currentValues = watchedValues || form.getFieldsValue(true);
    const normalizedDraft = normalizePayload(currentValues);
    const draftEnabled = normalizedDraft.enabled;
    const credentialsReady = prerequisitesReady(normalizedDraft, setting);
    const mappedCount = setting.mappedObjectCount ?? 0;
    const ttlHours = setting.expiresAfterHours ?? 0;

    return (
        <AdminPageFrame title="出站内镜" description="预算内的参考图片直接内联给上游，超出可内联上限的才镜像成公网地址" scroll>
            <div className="admin-settings-stack admin-egress-settings">
                <div className={cn("admin-egress-command-bar", dirty && "is-dirty")}>
                    <div className="admin-egress-command-copy">
                        <strong>{dirty ? "出站内镜有调整待保存" : "出站内镜配置已同步"}</strong>
                        <AdminStatusBadge label={dirty ? "尚未生效" : "服务端配置"} tone={dirty ? "warning" : "neutral"} />
                        <AdminStatusBadge label={draftEnabled ? "内镜已启用" : "内镜已停用"} tone={draftEnabled ? "success" : "neutral"} />
                    </div>
                    <div className="admin-egress-command-actions">
                        {dirty ? (
                            <Button icon={<RotateCcw className="size-4" />} disabled={saving} onClick={resetDraft}>
                                撤销调整
                            </Button>
                        ) : null}
                        <Button icon={<RefreshCw className="size-4" />} loading={refreshing} disabled={saving} onClick={requestRefresh}>
                            刷新状态
                        </Button>
                        <Button type="primary" icon={<Save className="size-4" />} loading={saving} disabled={!dirty || loading || refreshing} onClick={() => void submitSave()}>
                            保存修改
                        </Button>
                    </div>
                </div>

                {loadError || saveError ? (
                    <div className="flex items-start gap-2 rounded-lg border border-destructive/30 p-3 text-sm" role="alert">
                        <AlertTriangle className="size-4 shrink-0 text-destructive" aria-hidden="true" />
                        <span>{saveError || `${loadError}。页面仍显示上一次成功读取的配置。`}</span>
                    </div>
                ) : null}

                <Form
                    form={form}
                    layout="vertical"
                    requiredMark={false}
                    disabled={loading || refreshing || saving}
                    onValuesChange={(_, values: ProviderEgressForm) => {
                        let nextValues = values;
                        if (values.enabled && !prerequisitesReady(normalizePayload(values), setting)) {
                            form.setFieldValue("enabled", false);
                            nextValues = { ...values, enabled: false };
                        }
                        setDirty(hasChanges(nextValues, setting));
                        setSaveError("");
                    }}
                >
                    <SettingsSectionCard
                        icon={<KeyRound className="size-4" aria-hidden="true" />}
                        title="1. 配置内镜存储位置"
                        description="只在参考图片超出可内联预算时才会用到这里的桶，不会改变素材的主存储位置。"
                        status={<AdminStatusBadge label={credentialsReady ? "已配置" : "待配置"} tone={credentialsReady ? "success" : "neutral"} />}
                    >
                        <div className="grid gap-4 sm:grid-cols-2">
                            <Form.Item name="provider" label="存储类型" extra="仅支持阿里云 OSS、腾讯云 COS、七牛云 Kodo 和通用 S3。">
                                <Select options={providerOptions} placeholder="选择存储类型" />
                            </Form.Item>
                            <Form.Item name="region" label="Region" extra="阿里云与 COS 用于签名；七牛按区域选择接入点。">
                                <Input autoComplete="off" placeholder="oss-cn-beijing" />
                            </Form.Item>
                            <Form.Item name="endpoint" label="Endpoint">
                                <Input autoComplete="off" placeholder="oss-cn-beijing.aliyuncs.com" />
                            </Form.Item>
                            <Form.Item name="bucket" label="Bucket">
                                <Input autoComplete="off" placeholder="存放出站内镜对象的桶" />
                            </Form.Item>
                            <Form.Item name="accessKeyId" label="AccessKeyId">
                                <Input autoComplete="off" placeholder="仅保存在服务端" />
                            </Form.Item>
                            <Form.Item name="accessKeySecret" label={setting.hasAccessKeySecret ? `SecretKey（${configuredSecretText}）` : "SecretKey"} extra={setting.hasAccessKeySecret ? "AccessKeyId 不变时留空可保留原密钥。" : undefined}>
                                <Input.Password autoComplete="new-password" placeholder={setting.hasAccessKeySecret ? "留空保留原密钥" : "仅保存在服务端"} />
                            </Form.Item>
                            <Form.Item name="pathPrefix" label="对象前缀" extra="内镜对象固定落在该前缀下的 egress/ 子目录。">
                                <Input autoComplete="off" placeholder="open-ai-canvas" />
                            </Form.Item>
                            <Form.Item name="cdnBaseUrl" label="CDN 域名（可选）">
                                <Input autoComplete="off" placeholder="https://cdn.example.com" />
                            </Form.Item>
                            <Form.Item name="cdnAuthMode" label="CDN 鉴权方式" extra="选了公有读时，地址不带签名参数；其余情况回源签名。">
                                <Select options={cdnModeOptions} placeholder="不使用 CDN" />
                            </Form.Item>
                        </div>
                        <div className="flex flex-wrap items-center gap-2 pt-2">
                            <Button icon={<Zap className="size-4" />} loading={testing} disabled={saving || refreshing} onClick={() => void runTest()}>
                                测试连通性
                            </Button>
                            <span className="text-xs text-muted-foreground">测试会写入并读回一个极小探针对象，随后删除。</span>
                        </div>
                    </SettingsSectionCard>

                    <SettingsSectionCard
                        icon={<CloudUpload className="size-4" aria-hidden="true" />}
                        title="2. 是否启用出站内镜"
                        description="上游接受 Base64 内联图片，所以预算内的本地参考图片直接内联交付；只有超出可内联上限的才需要这里的桶。"
                        status={<AdminStatusBadge label={draftEnabled ? "已启用" : "已停用"} tone={draftEnabled ? "success" : "neutral"} />}
                    >
                        <div className="flex flex-wrap items-center justify-between gap-4">
                            <div>
                                <strong>镜像超出内联预算的参考图片</strong>
                                <p className="text-sm text-muted-foreground">关闭后，预算内的小图仍按 Base64 内联正常出图；只有超出可内联上限的图片会直接报“需要配置出站内镜”，不再静默下发上游取不到的内网地址。</p>
                            </div>
                            <div className="flex items-center gap-2">
                                <span>{draftEnabled ? "启用" : "停用"}</span>
                                <Form.Item name="enabled" valuePropName="checked" noStyle>
                                    <Switch aria-label="启用出站内镜，保存修改后生效" />
                                </Form.Item>
                            </div>
                        </div>
                        <ul className="mt-3 space-y-1 text-xs text-muted-foreground">
                            <li>可内联上限按上游契约留出余量；触发镜像时任务报错会写明本次图片合计字节与单张/合计上限。</li>
                            <li>镜像出去的素材在厂商访问地址有效期内可被公网读取{ttlHours ? `（当前有效期 ${ttlHours} 小时）` : ""}。</li>
                            <li>本期只交付图片素材（内联或镜像）；参考视频与参考音频仍要求上游可达地址。</li>
                        </ul>
                    </SettingsSectionCard>

                    <SettingsSectionCard
                        icon={<Eraser className="size-4" aria-hidden="true" />}
                        title="3. 复用映射与回收"
                        description="镜像出去的同一张图按内容指纹只上传一次；到期对象由后台清理任务按保留天数回收。内联交付的图片不占映射。"
                        status={<AdminStatusBadge label={`已映射 ${mappedCount} 个对象`} tone={mappedCount > 0 ? "info" : "neutral"} />}
                    >
                        <div className="flex flex-wrap items-center gap-2">
                            <Button icon={<Eraser className="size-4" />} disabled={saving || refreshing || mappedCount === 0} onClick={runClearMappings}>
                                删除对象并清空映射
                            </Button>
                            <span className="text-xs text-muted-foreground">桶被外部清空、换凭据或换了桶之后可以用；清空后的下一张图会重新上传。</span>
                        </div>
                    </SettingsSectionCard>
                </Form>
            </div>
        </AdminPageFrame>
    );
}

function toFormValues(setting: AdminProviderEgressSetting): ProviderEgressForm {
    return {
        enabled: setting.enabled,
        provider: setting.provider || "aliyun",
        region: setting.region || "",
        endpoint: setting.endpoint || "",
        bucket: setting.bucket || "",
        accessKeyId: setting.accessKeyId || "",
        accessKeySecret: "",
        pathPrefix: setting.pathPrefix || "",
        cdnBaseUrl: setting.cdnBaseUrl || "",
        cdnAuthMode: setting.cdnAuthMode || "",
    };
}

function normalizePayload(values: Partial<ProviderEgressForm>): ProviderEgressPayload {
    return {
        enabled: Boolean(values.enabled),
        provider: values.provider?.trim() || "",
        region: values.region?.trim() || "",
        endpoint: values.endpoint?.trim() || "",
        bucket: values.bucket?.trim() || "",
        accessKeyId: values.accessKeyId?.trim() || "",
        accessKeySecret: values.accessKeySecret?.trim() || "",
        pathPrefix: values.pathPrefix?.trim().replace(/^\/+|\/+$/g, "") || "",
        cdnBaseUrl: values.cdnBaseUrl?.trim().replace(/\/+$/, "") || "",
        cdnAuthMode: values.cdnAuthMode?.trim().toLowerCase() || "",
    };
}

function hasChanges(values: Partial<ProviderEgressForm>, setting: AdminProviderEgressSetting | null) {
    if (!setting) return false;
    const draft = normalizePayload(values);
    const saved = normalizePayload(toFormValues(setting));
    if (draft.accessKeySecret !== "") return true;
    return JSON.stringify({ ...draft, accessKeySecret: "" }) !== JSON.stringify({ ...saved, accessKeySecret: "" });
}

function usableSecret(values: ProviderEgressPayload, setting: AdminProviderEgressSetting) {
    return Boolean(values.accessKeySecret || (values.accessKeyId === setting.accessKeyId && setting.hasAccessKeySecret));
}

function prerequisitesReady(values: ProviderEgressPayload, setting: AdminProviderEgressSetting) {
    return Boolean(values.provider && values.endpoint && values.bucket && values.accessKeyId && usableSecret(values, setting));
}

function validateDraft(draft: ProviderEgressPayload, setting: AdminProviderEgressSetting) {
    // 关掉总闸不需要填完配置，与方舟素材库设置页的早退一致；否则管理员无法先把功能关掉。
    if (!draft.enabled) return "";
    if (!draft.provider) return "请选择出站内镜的存储类型";
    if (!draft.endpoint) return "请填写出站内镜的 Endpoint";
    if (!draft.bucket) return "请填写出站内镜的 Bucket";
    if (!draft.accessKeyId) return "请填写出站内镜的 AccessKeyId";
    if (!usableSecret(draft, setting)) return draft.accessKeyId !== setting.accessKeyId ? "更换 AccessKeyId 时请同时填写匹配的 SecretKey" : "请填写出站内镜的 SecretKey";
    if (draft.cdnAuthMode !== "" && draft.cdnAuthMode !== "public" && draft.cdnAuthMode !== "qiniu") return "出站内镜的 CDN 鉴权方式仅支持 public 或 qiniu";
    if (draft.cdnBaseUrl && !draft.cdnAuthMode) return "配置了 CDN 域名时必须选择 CDN 鉴权方式";
    return "";
}

function nextSecretPresence(values: ProviderEgressPayload, setting: AdminProviderEgressSetting) {
    if (values.accessKeySecret) return true;
    return values.accessKeyId === setting.accessKeyId && setting.hasAccessKeySecret;
}

function responseMatches(setting: AdminProviderEgressSetting, expected: ProviderEgressPayload, expectedHasSecret: boolean) {
    return (
        setting.enabled === expected.enabled &&
        setting.provider === expected.provider &&
        setting.region === expected.region &&
        setting.endpoint === expected.endpoint &&
        setting.bucket === expected.bucket &&
        setting.accessKeyId === expected.accessKeyId &&
        setting.pathPrefix === expected.pathPrefix &&
        setting.cdnBaseUrl === expected.cdnBaseUrl &&
        setting.cdnAuthMode === expected.cdnAuthMode &&
        setting.hasAccessKeySecret === expectedHasSecret
    );
}

function isProviderEgressSetting(value: unknown): value is AdminProviderEgressSetting {
    if (!value || typeof value !== "object") return false;
    const setting = value as Partial<AdminProviderEgressSetting>;
    return (
        typeof setting.enabled === "boolean" &&
        typeof setting.provider === "string" &&
        typeof setting.endpoint === "string" &&
        typeof setting.bucket === "string" &&
        typeof setting.accessKeyId === "string" &&
        typeof setting.hasAccessKeySecret === "boolean"
    );
}

export type AutomationCard = {
  id: number
  name: string
  apiUrl: string
  apiKey: string
  officialSite: string
  icon: string
  tint: string
  accent: string
  enabled: boolean
  // 模型白名单：声明 provider 支持的模型（精确或通配符）
  supportedModels?: Record<string, boolean>
  // 模型映射：external model -> internal model
  modelMapping?: Record<string, string>
  // 优先级分组：数字越小优先级越高（1-10，默认 1）
  level?: number
  // API 端点路径（可选）：覆盖平台默认端点
  apiEndpoint?: string
  // OpenAI 入口能力：auto / responses / chat_completions / both
  openAIEndpointMode?: string
  // Responses instructions 兼容：为缺失 instructions 的 Responses 请求补齐顶层 instructions
  bridgeResponsesInstructions?: boolean
  // Responses store=false 兼容：为要求 store=false 的 Responses 请求显式设置顶层 store=false
  forceResponsesStoreFalse?: boolean
  // Responses 丢弃字段列表：为不支持指定顶层字段的 Responses 请求在转发前移除对应字段
  dropResponsesFields?: string[]
  // [已废弃] Responses max_output_tokens 兼容：迁移到 dropResponsesFields
  dropResponsesMaxOutputTokens?: boolean
  // [已废弃] Responses temperature 兼容：迁移到 dropResponsesFields
  dropResponsesTemperature?: boolean
  // Images 丢弃字段列表：为不支持指定 JSON key 或 multipart field 的生图请求在转发前移除对应字段
  dropImageFields?: string[]
  // 异步生图模式：开启后自动走 创建任务（带 async=true）→轮询 /v1/tasks/{id}→转 OpenAI 格式
  // 适用于 duomiapi 等仅支持异步的生图上游，对客户端完全透明
  imageAsyncMode?: boolean
  // CLI 配置：存储供应商关联的 CLI 可编辑配置
  cliConfig?: Record<string, any>

  // === 可用性监控配置（新） ===
  // 可用性监控开关：是否启用后台健康检查
  availabilityMonitorEnabled?: boolean
  // 连通性自动拉黑：检测失败时是否自动拉黑该供应商
  connectivityAutoBlacklist?: boolean
  // 可用性高级配置：测试模型、端点和超时
  availabilityConfig?: {
    testModel?: string      // 测试用模型
    testEndpoint?: string   // 测试端点路径
    timeout?: number        // 超时时间（毫秒）
  }

  // === 旧连通性字段（已废弃，仅用于兼容旧数据） ===
  /** @deprecated 已迁移到 availabilityMonitorEnabled */
  connectivityCheck?: boolean
  /** @deprecated 已迁移到 availabilityConfig.testModel */
  connectivityTestModel?: string
  /** @deprecated 已迁移到 availabilityConfig.testEndpoint */
  connectivityTestEndpoint?: string
  /** @deprecated 已迁移到可用性配置中的认证方式 */
  connectivityAuthType?: string
  // 上游协议类型（anthropic / openai）
  upstreamProtocol?: string
}

export const automationCardGroups: Record<'claude' | 'codex' | 'gpt-image', AutomationCard[]> = {
  claude: [
    {
      id: 100,
      name: '0011',
      apiUrl: 'https://0011.ai',
      apiKey: '',
      officialSite: 'https://0011.ai',
      icon: 'aicoding',
      tint: 'rgba(10, 132, 255, 0.14)',
      accent: '#0aff5cff',
      enabled: false,
    },
    {
      id: 101,
      name: 'AICoding.sh',
      apiUrl: 'https://api.aicoding.sh',
      apiKey: '',
      officialSite: 'https://aicoding.sh',
      icon: 'aicoding',
      tint: 'rgba(10, 132, 255, 0.14)',
      accent: '#0a84ff',
      enabled: false,
    },
    {
      id: 102,
      name: 'Kimi',
      apiUrl: 'https://api.moonshot.cn/anthropic',
      apiKey: '',
      officialSite: 'https://kimi.moonshot.cn',
      icon: 'kimi',
      tint: 'rgba(16, 185, 129, 0.16)',
      accent: '#10b981',
      enabled: false,
    },
    {
      id: 103,
      name: 'Deepseek',
      apiUrl: 'https://api.deepseek.com/anthropic',
      apiKey: '',
      officialSite: 'https://www.deepseek.com',
      icon: 'deepseek',
      tint: 'rgba(251, 146, 60, 0.18)',
      accent: '#f97316',
      enabled: false,
    },
  ],
  codex: [
    {
      id: 201,
      name: 'AICoding.sh',
      apiUrl: 'https://api.aicoding.sh',
      apiKey: '',
      officialSite: 'https://www.aicoding.sh',
      icon: 'aicoding',
      tint: 'rgba(236, 72, 153, 0.16)',
      accent: '#ec4899',
      enabled: false,
    },
  ],
  'gpt-image': [
    {
      id: 401,
      name: 'GPT Image Provider',
      apiUrl: '',
      apiKey: '',
      officialSite: '',
      icon: 'openai',
      tint: 'rgba(16, 185, 129, 0.14)',
      accent: '#10b981',
      enabled: false,
      supportedModels: {
        'gpt-image-2': true,
      },
      apiEndpoint: '/v1/images/generations',
      connectivityAuthType: 'bearer',
    },
  ],
}

export function createAutomationCards(data: AutomationCard[] = []): AutomationCard[] {
  return data.map((item) => ({
    ...item,
    officialSite: item.officialSite ?? '',
    dropResponsesFields: Array.isArray(item.dropResponsesFields)
      ? item.dropResponsesFields
        .map((field) => field.trim())
        .filter(Boolean)
      : [],
    dropImageFields: Array.isArray(item.dropImageFields)
      ? item.dropImageFields
        .map((field) => field.trim())
        .filter(Boolean)
      : [],
  }))
}
